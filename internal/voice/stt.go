// stt.go — клиент Speech-to-Text по OpenAI-совместимому протоколу
// POST {base_url}/audio/transcriptions (multipart/form-data) → {"text": "..."}.
//
// Совместимые провайдеры:
//   - Groq:   base_url https://api.groq.com/openai/v1, модель whisper-large-v3-turbo
//   - OpenAI: base_url https://api.openai.com/v1, модель whisper-1 / gpt-4o-transcribe
//   - локально: whisper.cpp server (OpenAI-совместимый endpoint), Speaches, LocalAI
package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/archi/archi/internal/config"
)

// Transcriber — клиент STT с опциональным фолбэком (второй провайдер той же формы).
type Transcriber struct {
	cfg      config.VoiceSTTConfig
	log      *slog.Logger
	http     *http.Client
	fallback *Transcriber // основной упал (сеть/5xx/лимиты) — пробуем его
}

// NewTranscriber создаёт транскрайбер. cfg должен быть провалидирован (Enabled=true).
func NewTranscriber(cfg config.VoiceSTTConfig, log *slog.Logger) *Transcriber {
	timeout := cfg.Timeout.Duration
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	t := &Transcriber{
		cfg:  cfg,
		log:  log,
		http: &http.Client{Timeout: timeout},
	}
	if cfg.Fallback != nil && cfg.Fallback.BaseURL != "" {
		t.fallback = NewTranscriber(*cfg.Fallback, log)
	}
	return t
}

// Transcribe распознаёт речь в аудио-байтах. filename — исходное имя (важно
// расширение: .ogg/.oga от Telegram, .wav с микрофона, .mp3 из audio-сообщений).
func (t *Transcriber) Transcribe(ctx context.Context, audio []byte, filename string) (string, error) {
	if len(audio) == 0 {
		return "", errors.New("пустое аудио")
	}
	if t.cfg.MaxBytes > 0 && int64(len(audio)) > t.cfg.MaxBytes {
		return "", fmt.Errorf("аудио %d КБ превышает лимит %d КБ",
			len(audio)/1024, t.cfg.MaxBytes/1024)
	}

	// Облака (Groq/OpenAI) принимают ogg/opus и mp3 напрямую; локальный
	// whisper.cpp обычно хочет WAV 16 кГц — конвертируем через внешний ffmpeg.
	data, fname := audio, filename
	if t.needsWAV(filename) {
		if wav, err := ConvertToWAV16k(ctx, audio, filename); err != nil {
			t.log.Warn("voice: конверсия в WAV не удалась, отправляю оригинал", "err", err)
		} else {
			data, fname = wav, "audio.wav"
		}
	}

	// До 3 попыток: транскрипция голосового из Telegram — одноразовое событие,
	// терять его из-за ECONNRESET обидно.
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			t.log.Warn("voice: повтор STT", "attempt", attempt, "err", lastErr)
			select {
			case <-time.After(time.Duration(attempt) * time.Second):
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		text, retryable, err := t.transcribeOnce(ctx, data, fname)
		if err == nil {
			return text, nil
		}
		lastErr = err
		if !retryable {
			break // 4xx от основного (битый ключ/лимит) — ретраи бессмысленны, но фолбэк-провайдер может принять
		}
	}

	// Основной провайдер не ответил — пробуем резервный (обычно локальный whisper.cpp).
	if t.fallback != nil {
		t.log.Warn("voice: основной STT недоступен, переключаюсь на фолбэк", "err", lastErr,
			"fallback_url", t.fallback.cfg.BaseURL)
		text, ferr := t.fallback.Transcribe(ctx, audio, filename) // фолбэк сам решает про конверсию
		if ferr == nil {
			return text, nil
		}
		return "", fmt.Errorf("stt: основной: %v; фолбэк: %v", lastErr, ferr)
	}
	return "", fmt.Errorf("stt: исчерпаны попытки: %w", lastErr)
}

// transcribeOnce — один запрос к провайдеру. retryable=true для сетевых ошибок и 5xx.
func (t *Transcriber) transcribeOnce(ctx context.Context, data []byte, filename string) (string, bool, error) {
	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	fw, err := w.CreateFormFile("file", safeName(filename))
	if err != nil {
		return "", false, fmt.Errorf("multipart: %w", err)
	}
	if _, err := fw.Write(data); err != nil {
		return "", false, fmt.Errorf("multipart write: %w", err)
	}
	_ = w.WriteField("model", t.cfg.Model)
	_ = w.WriteField("response_format", "json")
	if t.cfg.Language != "" {
		_ = w.WriteField("language", t.cfg.Language)
	}
	if err := w.Close(); err != nil {
		return "", false, fmt.Errorf("multipart close: %w", err)
	}

	url := strings.TrimRight(t.cfg.BaseURL, "/") + "/audio/transcriptions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return "", false, fmt.Errorf("запрос: %w", err)
	}
	if t.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+t.cfg.APIKey)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := t.http.Do(req)
	if err != nil {
		return "", true, fmt.Errorf("сеть: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", true, fmt.Errorf("чтение ответа: %w", err)
	}
	if resp.StatusCode >= 500 {
		return "", true, fmt.Errorf("stt http %d: %s", resp.StatusCode, clipB(raw, 200))
	}
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("stt http %d: %s", resp.StatusCode, clipB(raw, 300))
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", false, fmt.Errorf("парсинг ответа: %w (фрагмент: %s)", err, clipB(raw, 200))
	}
	return strings.TrimSpace(out.Text), false, nil
}

// needsWAV — надо ли перекодировать в WAV перед отправкой.
func (t *Transcriber) needsWAV(filename string) bool {
	ext := strings.ToLower(filepath.Ext(filename))
	if ext == ".wav" {
		return false // уже WAV (запись с микрофона)
	}
	switch t.cfg.Convert {
	case "always":
		return true
	case "never":
		return false
	default: // "auto": локальный сервер (whisper.cpp и др.) обычно хочет WAV
		return isLocalURL(t.cfg.BaseURL)
	}
}

func clipB(b []byte, n int) string {
	s := string(b)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
