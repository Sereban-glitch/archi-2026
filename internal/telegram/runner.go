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

// Runner — связка бота с агентом: маршрутизация команд, голосовые, тревоги, сводки.
type Runner struct {
	bot    *Bot
	agent  *agent.Agent
	bridge *bridge.Bridge
	stt    *voice.Transcriber // nil → голосовые отключены
	tts    *voice.Synthesizer // nil → озвучка ответов отключена
	log    *slog.Logger
	tz     *time.Location

	digestCfg DigestSchedule
	once      sync.Once
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
		bot: bot, agent: a, bridge: b, stt: stt, tts: tts, log: log, tz: tz, digestCfg: digest,
	}
}

// Run запускает polling + подписки (тревоги, сводки). Блокирует до отмены ctx.
func (r *Runner) Run(ctx context.Context) {
	go r.watchEvents(ctx)
	go r.digestLoop(ctx)

	err := r.bot.Run(ctx, func(chatID, userID int64, text string, att *VoiceAttachment) {
		r.handleMessage(ctx, chatID, userID, text, att)
	})
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
	if r.tts != nil && err == nil && reply != "" {
		if max := r.tts.MaxChars(); max > 0 && len([]rune(reply)) > max {
			return // длинные простыни не озвучиваем — читаем глазами
		}
		audio, err := r.tts.Synthesize(actx, voice.PlainText(reply))
		if err != nil {
			r.log.Warn("tg: TTS не озвучил ответ", "err", err)
			return
		}
		if err := r.bot.SendVoice(actx, chatID, audio, 0); err != nil {
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
	return sb.String()
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
func (r *Runner) digestLoop(ctx context.Context) {
	if !r.digestCfg.Enabled || len(r.digestCfg.Times) == 0 {
		return
	}
	for {
		now := time.Now().In(r.tz)
		next := now.Add(24 * time.Hour)
		for _, t := range r.digestCfg.Times {
			var h, m int
			if _, err := fmt.Sscanf(t, "%d:%d", &h, &m); err != nil {
				continue
			}
			cand := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, r.tz)
			if cand.After(now) && cand.Before(next) {
				next = cand
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
