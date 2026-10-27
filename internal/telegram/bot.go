// Package telegram — собственный Telegram Bot API long-polling клиент
// с защитой от ECONNRESET/ECONNREFUSED и экспоненциальным backoff.
// Писать свой клиент вместо go-telegram-bot-api решили сознательно:
// контроль ретраев на транспортном уровне, ноль внешних зависимостей,
// точная обработка 409/429 и jittered backoff (уроки 2026: агент без
// устойчивого polling-канала — не агент, а генератор алертов).
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/archi/archi/internal/config"
)

// Update — входящее обновление Telegram (только нужные поля).
type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *TgMessage `json:"message,omitempty"`
}

// TgMessage — сообщение.
type TgMessage struct {
	MessageID int64    `json:"message_id"`
	From      *TgUser  `json:"from,omitempty"`
	Chat      TgChat   `json:"chat"`
	Text      string   `json:"text,omitempty"`
}

// TgUser — автор сообщения.
type TgUser struct {
	ID        int64  `json:"id"`
	Username  string `json:"username,omitempty"`
	FirstName string `json:"first_name,omitempty"`
}

// TgChat — чат.
type TgChat struct {
	ID int64 `json:"id"`
}

// apiResponse — конверт ответа Bot API.
type apiResponse struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description,omitempty"`
	ErrorCode   int             `json:"error_code,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
}

// Bot — long-polling клиент Telegram.
type Bot struct {
	token      string
	apiBase    string
	pollTimeout time.Duration
	allowed    map[int64]bool
	admins     map[int64]bool
	http       *http.Client
	log        *slog.Logger

	mu      sync.Mutex
	lastUpdate int64
	running  bool
}

// New создаёт бота.
func New(cfg config.TelegramConfig, log *slog.Logger) *Bot {
	allowed := make(map[int64]bool, len(cfg.Allowed))
	for _, id := range cfg.Allowed {
		allowed[id] = true
	}
	admins := make(map[int64]bool, len(cfg.Admins))
	for _, id := range cfg.Admins {
		admins[id] = true
	}
	tr := &http.Transport{
		MaxIdleConns:          4,
		IdleConnTimeout:       75 * time.Second,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: cfg.PollTimeout.Duration + 30*time.Second,
	}
	return &Bot{
		token:       cfg.Token,
		apiBase:     cfg.APIBase + "/bot" + cfg.Token,
		pollTimeout: cfg.PollTimeout.Duration,
		allowed:     allowed,
		admins:      admins,
		http:        &http.Client{Transport: tr},
		log:         log,
	}
}

// IsAllowed — авторизован ли пользователь.
func (b *Bot) IsAllowed(userID int64) bool { return b.allowed[userID] }

// IsAdmin — является ли админом.
func (b *Bot) IsAdmin(userID int64) bool { return b.admins[userID] }

// pollResponse — сырой HTTP запрос к Bot API с ретраями на транспортных ошибках.
func (b *Bot) pollResponse(ctx context.Context, method string, body any, maxRetries int) (json.RawMessage, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			// экспоненциальный backoff с джиттером: 1s, 2s, 4s, 8s… максимум 60s
			d := time.Duration(1<<uint(min(attempt-1, 6))) * time.Second
			if d > 60*time.Second {
				d = 60 * time.Second
			}
			d += time.Duration(rand.Int63n(int64(time.Second)))
			b.log.Warn("tg: повтор запроса", "method", method, "attempt", attempt, "backoff", d.String(), "err", lastErr)
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		raw, apiDesc, apiCode, err := b.doPost(ctx, method, payload)
		if err == nil {
			return raw, nil
		}
		lastErr = err
		// 409 Conflict: другой поллер с тем же токеном — ждём и пробуем снова
		if apiCode == 409 {
			continue
		}
		// 429: honor Retry-After (запакован в err как errTg429)
		var e429 *errTooManyRequests
		if errors.As(err, &e429) {
			select {
			case <-time.After(e429.retryAfter):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			continue
		}
		// прочие API-ошибки (4xx): не ретраебельны
		if apiDesc != "" {
			return nil, err
		}
		// транспортная ошибка: ретраим
	}
	return nil, fmt.Errorf("tg %s: исчерпаны попытки: %w", method, lastErr)
}

type errTooManyRequests struct {
	retryAfter time.Duration
	desc       string
}

func (e *errTooManyRequests) Error() string { return "429: " + e.desc }

// doPost — одна попытка. Возвращает (result, apiDescription, apiCode, err).
func (b *Bot) doPost(ctx context.Context, method string, payload []byte) (json.RawMessage, string, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		b.apiBase+"/"+method, bytes.NewReader(payload))
	if err != nil {
		return nil, "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.http.Do(req)
	if err != nil {
		// ECONNRESET и прочие сетевые — помечаем как транспортные (apiDesc пуст).
		if isNetRetryable(err) {
			return nil, "", 0, fmt.Errorf("сеть: %w", err)
		}
		return nil, "", 0, fmt.Errorf("запрос: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, "", 0, fmt.Errorf("чтение тела: %w", err)
	}
	var env apiResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, "", 0, fmt.Errorf("парсинг ответа: %w (фрагмент: %s)", err, clip(string(raw), 200))
	}
	if !env.OK {
		if env.ErrorCode == 429 {
			// достаём retry_after из параметров (упрощённо — по заголовку или 5с)
			ra := 5 * time.Second
			if v := resp.Header.Get("Retry-After"); v != "" {
				if sec, e := strconv.Atoi(v); e == nil {
					ra = time.Duration(sec) * time.Second
				}
			}
			return nil, "", 429, &errTooManyRequests{retryAfter: ra, desc: env.Description}
		}
		return nil, env.Description, env.ErrorCode,
			fmt.Errorf("tg api %d: %s", env.ErrorCode, env.Description)
	}
	return env.Result, "", 0, nil
}

// isNetRetryable — ретраебельные сетевые ошибки (уроки от node-бота: ECONNRESET — норма жизни).
func isNetRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EPIPE) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	if strings.Contains(err.Error(), "connection reset") || strings.Contains(err.Error(), "EOF") {
		return true
	}
	return false
}

// SendMessage отправляет текст (разбивая длинные на куски по 4000 символов, markdown-safe).
func (b *Bot) SendMessage(ctx context.Context, chatID int64, text string) error {
	for _, chunk := range splitMessage(text, 4000) {
		payload := map[string]any{
			"chat_id":    chatID,
			"text":       chunk,
			"parse_mode": "Markdown",
		}
		// первая попытка с Markdown; при parse-ошибке — повтор без форматирования
		_, _, _, err := b.doPost(ctx, "sendMessage", mustJSON(payload))
		if err != nil {
			payload["parse_mode"] = ""
			delete(payload, "parse_mode")
			if _, _, _, err2 := b.doPost(ctx, "sendMessage", mustJSON(payload)); err2 != nil {
				b.log.Error("tg: sendMessage не удался даже без markdown", "chat", chatID, "err", err2)
				return err2
			}
		}
	}
	return nil
}

// Run — цикл long polling. updatesFn вызывается на каждое текстовое сообщение.
func (b *Bot) Run(ctx context.Context, updatesFn func(chatID, userID int64, text string, isUpdate bool)) error {
	b.mu.Lock()
	if b.running {
		b.mu.Unlock()
		return errors.New("tg: polling уже запущен")
	}
	b.running = true
	b.mu.Unlock()

	b.log.Info("tg: polling запущен", "timeout", b.pollTimeout.String())

	// Приветственное сообщение не шлём (пусть первый юзер напишет сам).
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		offset := b.lastUpdate + 1
		payload := map[string]any{
			"offset":          offset,
			"timeout":         int(b.pollTimeout.Seconds()),
			"allowed_updates": []string{"message"},
		}
		raw, err := b.pollResponse(ctx, "getUpdates", payload, 6)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			b.log.Error("tg: getUpdates сдох, сплю 5с и продолжаю", "err", err)
			select {
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		}
		var updates []Update
		if err := json.Unmarshal(raw, &updates); err != nil {
			b.log.Warn("tg: битый getUpdates payload", "err", err)
			continue
		}
		for _, u := range updates {
			if u.UpdateID > b.lastUpdate {
				b.lastUpdate = u.UpdateID
			}
			if u.Message == nil || strings.TrimSpace(u.Message.Text) == "" {
				continue
			}
			msg := u.Message
			// авторизация: allowlist
			var uid int64
			if msg.From != nil {
				uid = msg.From.ID
			}
			if !b.IsAllowed(uid) {
				b.log.Warn("tg: сообщение от неавторизованного пользователя — игнор",
					"uid", uid, "username", userOf(msg.From))
				continue
			}
			go updatesFn(msg.Chat.ID, uid, msg.Text, false) // каждый апдейт — отдельная горутина
		}
	}
}

func userOf(u *TgUser) string {
	if u == nil {
		return "?"
	}
	if u.Username != "" {
		return "@" + u.Username
	}
	return u.FirstName
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func splitMessage(text string, limit int) []string {
	if len(text) <= limit {
		return []string{text}
	}
	var parts []string
	for len(text) > 0 {
		n := limit
		if n > len(text) {
			n = len(text)
		}
		// не режем внутри UTF-8 руны
		for n > 0 && !utf8Boundary(text, n) {
			n--
		}
		parts = append(parts, text[:n])
		text = text[n:]
	}
	return parts
}

func utf8Boundary(s string, i int) bool {
	if i >= len(s) {
		return true
	}
	b := s[i]
	return b < 0x80 || b >= 0xC0
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
