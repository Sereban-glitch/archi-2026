package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/archi/archi/internal/agent"
	"github.com/archi/archi/internal/bridge"
	"github.com/archi/archi/internal/voice"
)

// Runner — связка бота с агентом: маршрутизация команд, голосовые, тревоги, сводки,
// approval-клавиатуры для опасных команд.
type Runner struct {
	bot    *Bot
	agent  *agent.Agent
	bridge *bridge.Bridge
	stt    *voice.Transcriber // nil → голосовые отключены
	tts    *voice.Synthesizer // nil → озвучка ответов отключена
	log    *slog.Logger
	tz     *time.Location

	digestCfg DigestSchedule
	admins    []int64

	// approval: ожидающие решения (callback data «appr:<id>» → канал решения)
	apprMu   sync.Mutex
	apprWait map[string]chan bool
	apprSeq  int64
}

// DigestSchedule — параметры сводок.
type DigestSchedule struct {
	Enabled bool
	Times   []string // "09:00", "21:00"
	Chats   []int64
}

// NewRunner создаёт раннер. stt/tts опциональны (nil = функция выключена).
func NewRunner(bot *Bot, a *agent.Agent, b *bridge.Bridge, tz *time.Location,
	digest DigestSchedule, stt *voice.Transcriber, tts *voice.Synthesizer, log *slog.Logger) *Runner {
	return &Runner{
		bot: bot, agent: a, bridge: b, stt: stt, tts: tts, log: log, tz: tz,
		digestCfg: digest, apprWait: make(map[string]chan bool),
	}
}

// Ask — реализация tools.Approver: inline-клавиатура первому админу,
// ждём решения до отмены ctx (таймаут задаёт вызывающий — tools.confirm).
// Нет ответа/ошибки → false (fail-closed).
func (r *Runner) Ask(ctx context.Context, summary string) (bool, error) {
	if len(r.digestCfg.Chats) == 0 {
		return false, fmt.Errorf("не указаны админы для approval-клавиатуры (telegram.admins)")
	}
	chatID := r.digestCfg.Chats[0] // решение принимает первый админ

	r.apprMu.Lock()
	r.apprSeq++
	id := strconv.FormatInt(r.apprSeq, 36)
	ch := make(chan bool, 1)
	r.apprWait[id] = ch
	r.apprMu.Unlock()
	defer func() {
		r.apprMu.Lock()
		delete(r.apprWait, id)
		r.apprMu.Unlock()
	}()

	msgID, err := r.bot.SendKeyboard(ctx, chatID, summary, [][2]string{
		{"appr:" + id + ":yes", "✅ Разрешить"},
		{"appr:" + id + ":no", "❌ Отклонить"},
	})
	if err != nil {
		return false, fmt.Errorf("клавиатура не отправлена: %w", err)
	}

	select {
	case ok := <-ch:
		_ = r.bot.EditMessageText(ctx, chatID, msgID, summary+"\n\n"+map[bool]string{true: "✅ ОДОБРЕНО — исполняю…", false: "❌ ОТКЛОНЕНО"}[ok])
		return ok, nil
	case <-ctx.Done():
		_ = r.bot.EditMessageText(context.Background(), chatID, msgID, summary+"\n\n⏱ Время на решение истекло — команда НЕ исполнена (fail-closed).")
		return false, fmt.Errorf("владелец не ответил за отведённое время")
	}
}

// handleCallback — разбор нажатий inline-кнопок approval-клавиатур.
func (r *Runner) handleCallback(chatID, userID int64, data, callbackID string) {
	r.bot.AnswerCallback(context.Background(), callbackID, "")
	if !strings.HasPrefix(data, "appr:") {
		return
	}
	parts := strings.Split(data, ":")
	if len(parts) != 3 {
		return
	}
	id, verdict := parts[1], parts[2] == "yes"
	r.apprMu.Lock()
	ch, ok := r.apprWait[id]
	r.apprMu.Unlock()
	if !ok {
		return // уже отвечено/истекло — игнор
	}
	select {
	case ch <- verdict:
	default: // решение уже принято — не блокируемся
	}
}

// Run запускает polling + подписки (тревоги, сводки). Блокирует до отмены ctx.
func (r *Runner) Run(ctx context.Context) {
	go r.watchEvents(ctx)
	go r.digestLoop(ctx)

	err := r.bot.Run(ctx,
		func(chatID, userID int64, text string, att *VoiceAttachment) {
			r.handleMessage(ctx, chatID, userID, text, att)
		},
		r.handleCallback, // approval-кнопки
	)
	if err != nil && ctx.Err() == nil {
		r.log.Error("tg: polling остановлен с ошибкой", "err", err)
	}
}

// handleMessage — команды, свободный текст и голосовые сообщения.
func (r *Runner) handleMessage(ctx context.Context, chatID, userID int64, text string, att *VoiceAttachment) {
	defer func() {
		if rec := recover(); rec != nil {
			r.log.Error("tg: паника обработчика восстановлена", "panic", rec, "chat", chatID)
			_ = r.bot.SendMessage(ctx, chatID, "⚠️ Внутренняя ошибка при обработке сообщения (восстановлено).")
		}
	}()

	if att != nil {
		r.handleVoice(ctx, chatID, att)
		return
	}

	sessionID := "tg:" + strconv.FormatInt(chatID, 10)

	switch {
	case text == "/start" || text == "/help":
		_ = r.bot.SendMessage(ctx, chatID, helpText)
	case text == "/status":
		_ = r.bot.SendMessage(ctx, chatID, r.statusText())
	case text == "/reset":
		r.agent.Sessions().Reset(sessionID)
		_ = r.bot.SendMessage(ctx, chatID, "🧹 Контекст сессии сброшен.")
	case strings.HasPrefix(text, "/"):
		_ = r.bot.SendMessage(ctx, chatID, "Неизвестная команда. Доступны: /status, /reset, /help — остальное просто пиши текстом или голосом.")
	default:
		// основной путь — ReAct-агент
		r.replyWithAgent(ctx, chatID, text, "")
	}
}

// handleVoice — голосовое сообщение: скачивание → STT → агент → (TTS).
func (r *Runner) handleVoice(ctx context.Context, chatID int64, att *VoiceAttachment) {
	if r.stt == nil {
		_ = r.bot.SendMessage(ctx, chatID,
			"🎙 Голосовые сообщения отключены в конфиге (voice.stt.enabled=false).")
		return
	}

	// Пусть владелец сразу видит, что мы работаем, а не зависли.
	r.bot.SendChatAction(ctx, chatID, "typing")

	audio, err := r.bot.DownloadAttachment(ctx, att, maxVoiceBytes)
	if err != nil {
		r.log.Error("tg: не скачать голосовое", "err", err, "kind", att.Kind)
		_ = r.bot.SendMessage(ctx, chatID, "⚠️ Не удалось скачать аудио из Telegram: "+err.Error())
		return
	}

	sttCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	transcript, err := r.stt.Transcribe(sttCtx, audio, att.FileName)
	cancel()
	if err != nil {
		r.log.Error("tg: STT не распознал голосовое", "err", err, "bytes", len(audio))
		_ = r.bot.SendMessage(ctx, chatID, "⚠️ Распознавание не удалось: "+err.Error())
		return
	}
	if transcript == "" {
		_ = r.bot.SendMessage(ctx, chatID, "🎙 Речь не распознана (пустая транскрипция) — попробуй перезаписать.")
		return
	}

	// Ответ агента с цитатой транскрипта — чтобы было видно, что именно услышал STT.
	r.replyWithAgent(ctx, chatID, transcript, transcript)
}

// replyWithAgent — прогоняет текст через ReAct-цикл и отвечает в чат.
// echo — транскрипт голосового (для цитаты в ответе); пустая строка для текстовых.
func (r *Runner) replyWithAgent(ctx context.Context, chatID int64, text, echo string) {
	sessionID := "tg:" + strconv.FormatInt(chatID, 10)

	actx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	reply, err := r.agent.Handle(actx, sessionID, text)
	if err != nil {
		r.log.Error("tg: агент вернул ошибку", "err", err)
	}

	if echo != "" {
		if runes := []rune(echo); len(runes) > 200 {
			echo = string(runes[:200]) + "…"
		}
		reply = "🎙 «" + echo + "»\n\n" + reply
	}
	_ = r.bot.SendMessage(ctx, chatID, reply)

	// Голосовой ответ (TTS): только если включён и ответ достаточно короткий.
	// ctx (не actx!): actx мог быть почти исчерпан ReAct-циклом (аудит 2026-10).
	if r.tts != nil && err == nil && reply != "" {
		if max := r.tts.MaxChars(); max > 0 && len([]rune(reply)) > max {
			return // длинные простыни не озвучиваем — читаем глазами
		}
		// Нормализация «под речь» (числа/единицы/markdown) происходит
		// внутри Synthesize — 2026.15 «Живой голос».
		audio, err := r.tts.Synthesize(ctx, reply)
		if err != nil {
			r.log.Warn("tg: TTS не озвучил ответ", "err", err)
			return
		}
		if err := r.bot.SendVoice(ctx, chatID, audio, 0); err != nil {
			r.log.Warn("tg: sendVoice не прошёл", "err", err)
		}
	}
}

// statusText — быстрый статус без LLM (данные моста).
func (r *Runner) statusText() string {
	snap := r.bridge.Snapshot()
	if snap == nil {
		return "⏳ Телеметрия ещё не собрана — повтори через минуту."
	}
	var sb strings.Builder
	sb.WriteString("📡 Состояние узлов:\n")
	for _, pair := range []struct {
		title string
		m     *bridge.NodeMetrics
	}{{"💻 local", snap.Local}, {"☁️ remote", snap.Remote}} {
		if pair.m == nil {
			continue
		}
		if !pair.m.Online {
			fmt.Fprintf(&sb, "%s %s: 🔴 недоступен — %s\n", pair.title, pair.m.Hostname, pair.m.Error)
			continue
		}
		fmt.Fprintf(&sb, "%s %s: 🟢 CPU %.0f%% | RAM %.1f%% | диск %.0f%% | load %.2f | up %s\n",
			pair.title, pair.m.Hostname, pair.m.CPUPercent, pair.m.MemPercent(),
			pair.m.DiskPercent(), pair.m.Load1, humanDur(pair.m.UptimeSec))
	}
	if r.agent != nil {
		req, inT, outT := r.agent.UsageStats()
		fmt.Fprintf(&sb, "🧠 LLM с старта: запросов %d, токенов in %d / out %d\n", req, inT, outT)
	}
	return sb.String()
}

// NotifyAdmins — рассылка текста всем админам (реализация cron.Notifier).
func (r *Runner) NotifyAdmins(ctx context.Context, text string) {
	for _, id := range r.digestCfg.Chats {
		_ = r.bot.SendMessage(ctx, id, text)
	}
}

// watchEvents — тревоги моста → админам.
func (r *Runner) watchEvents(ctx context.Context) {
	events := r.bridge.Subscribe()
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-events:
			r.log.Info("event → telegram", "type", e.Type, "node", e.Node, "msg", e.Message)
			for _, id := range r.digestCfg.Chats {
				_ = r.bot.SendMessage(ctx, id, e.Message)
			}
		}
	}
}

// digestLoop — сводки в заданное время.
// Фикс 2026-10: раньше слоты искались только сегодня, и вечером следующий
// огонь смещался на ровно +24ч, пропуская завтрашнее утро (P1-аудита).
func (r *Runner) digestLoop(ctx context.Context) {
	if !r.digestCfg.Enabled || len(r.digestCfg.Times) == 0 {
		return
	}
	for {
		now := time.Now().In(r.tz)
		next := now.Add(24 * time.Hour)
		for day := 0; day < 2; day++ { // сегодня и завтра — ближайший слот всегда найдётся
			for _, t := range r.digestCfg.Times {
				var h, m int
				if _, err := fmt.Sscanf(t, "%d:%d", &h, &m); err != nil {
					continue
				}
				cand := time.Date(now.Year(), now.Month(), now.Day()+day, h, m, 0, 0, r.tz)
				if cand.After(now) && cand.Before(next) {
					next = cand
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(next)):
			for _, id := range r.digestCfg.Chats {
				_ = r.bot.SendMessage(ctx, id, r.statusText())
			}
		}
	}
}

// maxVoiceBytes — лимит на скачивание аудио из Bot API (20 МБ — потолок Telegram).
const maxVoiceBytes = 20 << 20

const helpText = `🤖 *Archi 2026* — гибридный DevOps-агент.

Мозг: 💻 ноутбук (Linux Mint). Руки: ☁️ GCP VM agrobot-prod.

*Команды:*
/status — состояние обоих узлов
/reset — сбросить контекст диалога
/help — эта справка

Остальное — обычным текстом или 🎙 голосовым сообщением: «проверь логи console-bot», «сколько RAM на сервере?», «перезапусти info-bot».`

func humanDur(sec uint64) string {
	d := time.Duration(sec) * time.Second
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm%ds", int(d.Minutes())%60, int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}
