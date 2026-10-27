package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/archi/archi/internal/bridge"
	"github.com/archi/archi/internal/config"
)

// ToolBackend — контракт реестра инструментов (реализуется tools.Registry).
// Интерфейс здесь, а не в tools, чтобы не создавать цикл импортов.
type ToolBackend interface {
	Definitions() []map[string]any
	Call(ctx context.Context, name string, input json.RawMessage) (result string, isError bool)
}

// Agent — ReAct-ядро: принимает запрос, гоняет цикл Think→Action→Observation,
// возвращает финальный текстовый ответ.
type Agent struct {
	cfg      config.AgentConfig
	llm      *Client
	tools    ToolBackend
	sessions *SessionManager
	mem      *Memory
	snap     bridge.SnapshotFn
	tz       *time.Location
	log      *slog.Logger

	// счётчики потребления (для /status и IPC: наблюдаемость без Prometheus)
	inTokens  atomic.Int64
	outTokens atomic.Int64
	requests  atomic.Int64
}

// New создаёт агента. historyLimit прокидывается в сессии (лимит сообщений истории).
func New(cfg config.AgentConfig, llm *Client, tools ToolBackend, mem *Memory,
	snap bridge.SnapshotFn, tz *time.Location, log *slog.Logger) *Agent {
	return &Agent{
		cfg: cfg, llm: llm, tools: tools, mem: mem, snap: snap, tz: tz, log: log,
		sessions: NewSessionManager(50, cfg.HistoryLimit, cfg.IdleSessionTTL.Duration, log),
	}
}

// Sessions открывает менеджер сессий (для /reset в Telegram).
func (a *Agent) Sessions() *SessionManager { return a.sessions }

// UsageStats — кумулятивное потребление LLM с старта демона.
func (a *Agent) UsageStats() (req, inT, outT int64) {
	return a.requests.Load(), a.inTokens.Load(), a.outTokens.Load()
}

// Handle — точка входа для Telegram/IPC: sessionID вида "tg:12345" или "cli".
// Циклы одной сессии сериализуются (Telegram шлёт апдейты параллельными горутинами):
// это устраняет гонку compact() против параллельных Append (TOCTOU, аудит 2026-10).
func (a *Agent) Handle(ctx context.Context, sessionID, userText string) (string, error) {
	s := a.sessions.Get(sessionID)

	s.SetBusy()
	defer s.ClearBusy()
	s.HandleMu().Lock()
	defer s.HandleMu().Unlock()

	s.Append(Message{Role: "user", Content: []ContentBlock{{Type: "text", Text: userText}}})

	if s.Chars() > a.cfg.CompactAfter {
		a.compact(ctx, s)
	}
	reply, err := a.reactLoop(ctx, s)
	if err != nil {
		// Graceful fallback: возвращаем лучший частичный результат, а не пустоту.
		if last := s.LastText(); last != "" {
			return fmt.Sprintf("%s\n\n⚠️ Ответ оборван: %v", last, err), nil
		}
		return fmt.Sprintf("⚠️ Не смог обработать запрос: %v\nПопробуй повторить позже или упрости задачу.", err), nil
	}
	return reply, nil
}

// reactLoop — сердце агента: цикл LLM → tool_use → tool_result → … до текста.
func (a *Agent) reactLoop(ctx context.Context, s *Session) (string, error) {
	defs := a.tools.Definitions()
	memHints := a.mem.Recall(lastUserQuery(s), a.memLimit())

	repeatGuard := newRepeatGuard(3)
	toolSem := make(chan struct{}, max(1, a.cfg.MaxToolParallel))

	for step := 1; step <= a.cfg.MaxSteps; step++ {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		system := BuildSystemPrompt(a.tz, a.snap(), memHints)
		resp, err := a.llm.SendMessage(ctx, system, s.History(), defs)
		if err != nil {
			return "", fmt.Errorf("шаг %d: %w", step, err)
		}
		a.requests.Add(1)
		a.inTokens.Add(int64(resp.Usage.InputTokens))
		a.outTokens.Add(int64(resp.Usage.OutputTokens))
		s.Append(Message{Role: "assistant", Content: resp.Content})

		// Собираем tool_use блоки.
		var calls []ContentBlock
		var lastText strings.Builder
		for _, b := range resp.Content {
			if b.Type == "tool_use" {
				calls = append(calls, b)
			} else if b.Type == "text" {
				lastText.WriteString(b.Text)
			}
		}
		if len(calls) == 0 {
			text := strings.TrimSpace(lastText.String())
			// Обрезанный ответ — честно предупреждаем (stop_reason=max_tokens молчал, аудит 2026-10)
			if resp.StopReason == "max_tokens" {
				text += "\n\n⚠️ Ответ обрезан лимитом max_tokens — попроси продолжить, если нужно."
			}
			return text, nil
		}

		// Исполняем инструменты (параллельно, с сохранением порядка результатов).
		results := make([]ContentBlock, len(calls))
		var wg sync.WaitGroup
		for i, call := range calls {
			wg.Add(1)
			go func(i int, call ContentBlock) {
				defer wg.Done()
				select {
				case toolSem <- struct{}{}:
				case <-ctx.Done():
					results[i] = toolResult(call.ID, "контекст отменён", true)
					return
				}
				defer func() { <-toolSem }()

				if repeatGuard.seen(call) {
					results[i] = toolResult(call.ID,
						"ОШИБКА: ты повторяешь идентичный вызов инструмента. Измени параметры, выбери другой путь или заверши задачу текстом.", true)
					return
				}
				out, isErr := a.tools.Call(ctx, call.Name, call.Input)
				results[i] = toolResult(call.ID, out, isErr)
			}(i, call)
		}
		wg.Wait()

		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		s.Append(Message{Role: "user", Content: results})

		if txt := strings.TrimSpace(lastText.String()); txt != "" {
			a.log.Info("react: промежуточная мысль", "step", step, "tools", len(calls), "text_len", len(txt))
		}
	}
	return fmt.Sprintf("⛔ Достигнут лимит шагов (%d). Промежуточный итог:\n%s",
		a.cfg.MaxSteps, s.LastText()), nil
}

func toolResult(id, content string, isErr bool) ContentBlock {
	return ContentBlock{Type: "tool_result", ToolUseID: id, Content: content, IsError: isErr}
}

// compact — компактизация истории: старые сообщения → LLM-сводка (handoff),
// при отказе LLM — наивное усечение (память не теряем полностью: сохраняем в Memory).
func (a *Agent) compact(ctx context.Context, s *Session) {
	hist := s.History()
	if len(hist) <= 6 {
		return
	}
	old := hist[:len(hist)-4]
	keep := hist[len(hist)-4:]

	var dump strings.Builder
	for _, m := range old {
		for _, b := range m.Content {
			switch b.Type {
			case "text":
				fmt.Fprintf(&dump, "[%s] %s\n", m.Role, b.Text)
			case "tool_use":
				fmt.Fprintf(&dump, "[%s tool:%s] %s\n", m.Role, b.Name, truncate(b.Input, 300))
			case "tool_result":
				fmt.Fprintf(&dump, "[observation] %s\n", truncate([]byte(b.Content), 300))
			}
		}
	}

	sum, err := a.llm.Summarize(ctx, dump.String())
	if err != nil || strings.TrimSpace(sum) == "" {
		a.log.Warn("compact: LLM-сводка не удалась, наивное усечение", "err", err)
		sum = truncate([]byte(dump.String()), 2000)
	}
	_ = a.mem.Save("handoff", "Сводка сессии "+s.ID+": "+sum, "session:"+s.ID)

	s.ReplaceHistory([]Message{
		{Role: "user", Content: []ContentBlock{{Type: "text",
			Text: "[КОНТЕКСТ: предыдущая история сжата]\n" + sum}}},
		{Role: "assistant", Content: []ContentBlock{{Type: "text", Text: "Принято, продолжаю с этим контекстом."}}},
	})
	s.Append2(keep...)
	a.log.Info("compact: история сжата", "session", s.ID, "summary_chars", len(sum))
}

// Append2 — внутренний helper (вставка сохранённого хвоста истории).
func (s *Session) Append2(msgs ...Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = append(s.history, msgs...)
}

// truncate — байтовое усечение с уважением границ UTF-8 рун.
func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	// прямой скан: end всегда стоит на границе руны
	end := 0
	for end < n {
		_, size := utf8.DecodeRune(b[end:])
		if end+size > n {
			break // разрез попал внутрь руны — не берём её
		}
		end += size
	}
	return string(b[:end]) + "…"
}

func lastUserQuery(s *Session) string {
	h := s.History()
	for i := len(h) - 1; i >= 0; i-- {
		if h[i].Role == "user" {
			for _, b := range h[i].Content {
				if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
					return b.Text
				}
			}
		}
	}
	return ""
}

func (a *Agent) memLimit() int { return 6 }

// repeatGuard — защита от зацикливания: детект идентичных повторных вызовов.
type repeatGuard struct {
	mu     sync.Mutex
	limit  int
	counts map[string]int
}

func newRepeatGuard(limit int) *repeatGuard {
	return &repeatGuard{limit: limit, counts: make(map[string]int)}
}

func (g *repeatGuard) seen(call ContentBlock) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	key := call.Name + "|" + string(call.Input)
	g.counts[key]++
	return g.counts[key] >= g.limit
}
