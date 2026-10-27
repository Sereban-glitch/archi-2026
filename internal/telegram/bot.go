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
	"mime/multipart"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/archi/archi/internal/config"
)

// Update — входящее обновление Telegram (только нужные поля).
type Update struct {
	UpdateID int64      `json:"update_id"`
	Message  *TgMessage `json:"message,omitempty"`
}

// TgMessage — сообщение.
type TgMessage struct {
	MessageID int64    `json:"message_id"`
	From      *TgUser  `json:"from,omitempty"`
	Chat      TgChat   `json:"chat"`
	Text      string   `json:"text,omitempty"`
	Voice     *TgVoice `json:"voice,omitempty"` // голосовое сообщение (OGG/Opus)
	Audio     *TgAudio `json:"audio,omitempty"` // аудиофайл (mp3 и пр.)
}

// TgVoice — голосовое сообщение Telegram (OGG/Opus с кодеком OPUS).
type TgVoice struct {
	FileID   string `json:"file_id"`
	Duration int    `json:"duration,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
}

// TgAudio — музыкальный/аудио файл (напр. mp3-диктовка).
type TgAudio struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name,omitempty"`
	Duration int    `json:"duration,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
}

// VoiceAttachment — нормализованный голосовой/аудио вход для раннера.
type VoiceAttachment struct {
	FileID   string
	FileName string // для audio — исходное имя; для voice — "voice.ogg"
	Kind     string // "voice" | "audio"
	Duration int
	Size     int64
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
	token       string
	apiBase     string
	fileBase    string // https://api.telegram.org/file/bot<token> — скачивание файлов
	pollTimeout time.Duration
	allowed     map[int64]bool
	admins      map[int64]bool
	http        *http.Client
	log         *slog.Logger

	mu         sync.Mutex
	lastUpdate int64
	running    bool
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
		fileBase:    cfg.APIBase + "/file/bot" + cfg.Token,
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

// doPost — одна попытка с JSON-телом.
func (b *Bot) doPost(ctx context.Context, method string, payload []byte) (json.RawMessage, string, int, error) {
	return b.postRaw(ctx, method, "application/json", bytes.NewReader(payload))
}

// postRaw — одна попытка с произвольным телом (JSON или multipart).
func (b *Bot) postRaw(ctx context.Context, method, contentType string, body io.Reader) (json.RawMessage, string, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.apiBase+"/"+method, body)
	if err != nil {
		return nil, "", 0, err
	}
	req.Header.Set("Content-Type", contentType)

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
// Идёт через pollResponse: ретраи на ECONNRESET/429/409 — те же, что у getUpdates.
func (b *Bot) SendMessage(ctx context.Context, chatID int64, text string) error {
	for _, chunk := range splitMessage(text, 4000) {
		payload := map[string]any{
			"chat_id":    chatID,
			"text":       chunk,
			"parse_mode": "Markdown",
		}
		if _, err := b.pollResponse(ctx, "sendMessage", payload, 3); err == nil {
			continue
		}
		// не прошло (в т.ч. непарсируемая разметка) — повтор без markdown
		delete(payload, "parse_mode")
		if _, err2 := b.pollResponse(ctx, "sendMessage", payload, 3); err2 != nil {
			b.log.Error("tg: sendMessage не удался даже без markdown", "chat", chatID, "err", err2)
			return err2
		}
	}
	return nil
}

// GetFile — file_path для скачивания по file_id (Bot API getFile).
func (b *Bot) GetFile(ctx context.Context, fileID string) (string, error) {
	raw, err := b.pollResponse(ctx, "getFile", map[string]any{"file_id": fileID}, 3)
	if err != nil {
		return "", err
	}
	var f struct {
		FilePath string `json:"file_path"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return "", fmt.Errorf("getFile parse: %w", err)
	}
	if f.FilePath == "" {
		return "", errors.New("getFile: пустой file_path")
	}
	return f.FilePath, nil
}

// DownloadFileBytes скачивает содержимое файла по file_path (с ретраями).
func (b *Bot) DownloadFileBytes(ctx context.Context, filePath string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		maxBytes = 20 << 20
	}
	// file_path отдаёт сам Telegram, но на всякий случай режем явно кривые.
	if filePath == "" || strings.Contains(filePath, "..") || strings.Contains(filePath, "://") {
		return nil, fmt.Errorf("некорректный file_path %q", filePath)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.fileBase+"/"+filePath, nil)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt) * time.Second):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		resp, err := b.http.Do(req)
		if err != nil {
			if !isNetRetryable(err) {
				return nil, err
			}
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if resp.StatusCode == 429 {
				lastErr = fmt.Errorf("file http 429")
				continue
			}
			return nil, fmt.Errorf("file http %d: %s", resp.StatusCode, clip(string(raw), 200))
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
		resp.Body.Close()
		if err != nil {
			lastErr = err // обрыв соединения посреди файла — ретраим
			continue
		}
		if int64(len(data)) > maxBytes {
			return nil, fmt.Errorf("файл больше лимита %d КБ", maxBytes/1024)
		}
		return data, nil
	}
	return nil, lastErr
}

// DownloadAttachment — getFile + скачивание одним вызовом.
func (b *Bot) DownloadAttachment(ctx context.Context, att *VoiceAttachment, maxBytes int64) ([]byte, error) {
	if att == nil || att.FileID == "" {
		return nil, errors.New("пустое вложение")
	}
	fp, err := b.GetFile(ctx, att.FileID)
	if err != nil {
		return nil, err
	}
	return b.DownloadFileBytes(ctx, fp, maxBytes)
}

// SendVoice отправляет голосовое сообщение (multipart, OGG/Opus).
func (b *Bot) SendVoice(ctx context.Context, chatID int64, audio []byte, duration int) error {
	if len(audio) == 0 {
		return nil
	}
	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	fw, err := w.CreateFormFile("voice", "voice.ogg")
	if err != nil {
		return err
	}
	if _, err := fw.Write(audio); err != nil {
		return err
	}
	_ = w.WriteField("chat_id", strconv.FormatInt(chatID, 10))
	if duration > 0 {
		_ = w.WriteField("duration", strconv.Itoa(duration))
	}
	if err := w.Close(); err != nil {
		return err
	}
	payload := append([]byte(nil), body.Bytes()...) // копия на каждую попытку
	ct := w.FormDataContentType()

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt) * time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		_, apiDesc, _, err := b.postRaw(ctx, "sendVoice", ct, bytes.NewReader(payload))
		if err == nil {
			return nil
		}
		lastErr = err
		if apiDesc != "" {
			return err // API-ошибка (4xx) не ретраится
		}
	}
	return lastErr
}

// SendChatAction — индикатор активности («typing», «record_voice»…), fire-and-forget.
func (b *Bot) SendChatAction(ctx context.Context, chatID int64, action string) {
	payload := map[string]any{"chat_id": chatID, "action": action}
	if _, _, _, err := b.doPost(ctx, "sendChatAction", mustJSON(payload)); err != nil {
		b.log.Debug("tg: sendChatAction не прошёл", "err", err)
	}
}

// Run — цикл long polling. updatesFn вызывается на каждое сообщение
// (текстовое — с text, голосовое/аудио — с att).
func (b *Bot) Run(ctx context.Context, updatesFn func(chatID, userID int64, text string, att *VoiceAttachment)) error {
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
			msg := u.Message
			if msg == nil {
				continue
			}
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
			// голосовое / аудио вложение → транскрипция в раннере
			if att := voiceAttachmentOf(msg); att != nil {
				go updatesFn(msg.Chat.ID, uid, "", att)
				continue
			}
			if strings.TrimSpace(msg.Text) == "" {
				continue // стикеры, фото и прочее — мимо
			}
			go updatesFn(msg.Chat.ID, uid, msg.Text, nil) // каждый апдейт — отдельная горутина
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

// voiceAttachmentOf — нормализует voice/audio часть сообщения (nil если это не аудио).
func voiceAttachmentOf(m *TgMessage) *VoiceAttachment {
	if m == nil {
		return nil
	}
	if m.Voice != nil {
		return &VoiceAttachment{
			FileID:   m.Voice.FileID,
			FileName: "voice.ogg", // Telegram шлёт OGG/Opus
			Kind:     "voice",
			Duration: m.Voice.Duration,
			Size:     m.Voice.FileSize,
		}
	}
	if m.Audio != nil {
		name := m.Audio.FileName
		if name == "" || filepath.Ext(name) == "" {
			name = "audio.mp3"
		}
		return &VoiceAttachment{
			FileID:   m.Audio.FileID,
			FileName: name,
			Kind:     "audio",
			Duration: m.Audio.Duration,
			Size:     m.Audio.FileSize,
		}
	}
	return nil
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
