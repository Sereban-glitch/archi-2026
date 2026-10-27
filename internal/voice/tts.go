// tts.go — опциональный синтез речи (Text-to-Speech) через OpenAI-совместимый
// POST {base_url}/audio/speech → бинарные аудио-данные.
//
// Совместимые провайдеры: OpenAI (gpt-4o-mini-tts / tts-1), Groq (playai-tts).
// Формат по умолчанию — opus (OGG/Opus): именно его ждёт Telegram sendVoice,
// локально играется через ffplay. Ответы длиннее max_chars не озвучиваются —
// пользователь получает их текстом.
package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/archi/archi/internal/config"
)

// Synthesizer — клиент TTS. nil-safe: везде в коде проверяется на nil.
type Synthesizer struct {
	cfg  config.VoiceTTSConfig
	log  *slog.Logger
	http *http.Client
}

// NewSynthesizer создаёт синтезатор.
func NewSynthesizer(cfg config.VoiceTTSConfig, log *slog.Logger) *Synthesizer {
	timeout := cfg.Timeout.Duration
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &Synthesizer{cfg: cfg, log: log, http: &http.Client{Timeout: timeout}}
}

// MaxChars — лимит длины текста для озвучки (0 = без лимита).
func (s *Synthesizer) MaxChars() int {
	if s == nil {
		return 0
	}
	return s.cfg.MaxChars
}

// PlainText грубо убирает markdown-разметку из ответа агента перед озвучкой.
// Точная разметка синтезатору не нужна — только чистый текст.
func PlainText(s string) string {
	return strings.NewReplacer("*", "", "`", "", "_", "", "#", "", ">", "").Replace(s)
}

// Synthesize превращает текст в аудио (формат — по конфигу, обычно OGG/Opus).
func (s *Synthesizer) Synthesize(ctx context.Context, text string) ([]byte, error) {
	if s == nil {
		return nil, errors.New("tts отключён")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, errors.New("пустой текст")
	}
	if s.cfg.MaxChars > 0 {
		r := []rune(text)
		if len(r) > s.cfg.MaxChars {
			text = string(r[:s.cfg.MaxChars])
		}
	}

	payload, err := json.Marshal(map[string]any{
		"model":           s.cfg.Model,
		"input":           text,
		"voice":           s.cfg.Voice,
		"response_format": s.cfg.Format,
	})
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(s.cfg.BaseURL, "/") + "/audio/speech"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("запрос: %w", err)
	}
	if s.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.cfg.APIKey)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("сеть: %w", err)
	}
	defer resp.Body.Close()

	// Успех = аудио-данные; JSON в теле — всегда ошибка провайдера.
	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/json") {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, fmt.Errorf("tts http %d: %s", resp.StatusCode, clipB(raw, 300))
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("tts http %d: %s", resp.StatusCode, clipB(raw, 300))
	}
	audio, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("чтение аудио: %w", err)
	}
	if len(audio) == 0 {
		return nil, errors.New("tts: пустой ответ")
	}
	return audio, nil
}

// Play воспроизводит аудио-байты на ноутбуке: ffplay (ffmpeg) → paplay → aplay.
// Используется командой `archi voice --speak`.
func Play(ctx context.Context, audio []byte, ext string, log *slog.Logger) error {
	tmp, err := writeTemp(audio, ext)
	if err != nil {
		return err
	}
	defer removeTemp(tmp)

	type player struct {
		bin  string
		args []string
	}
	var candidates []player
	if p, err := lookCommand("ffplay"); err == nil {
		candidates = append(candidates, player{p, []string{"-autoexit", "-nodisp", "-loglevel", "quiet", tmp}})
	}
	if p, err := lookCommand("paplay"); err == nil {
		candidates = append(candidates, player{p, []string{tmp}})
	}
	if p, err := lookCommand("aplay"); err == nil && ext == ".wav" {
		candidates = append(candidates, player{p, []string{"-q", tmp}})
	}
	if len(candidates) == 0 {
		return errors.New("нет плеера: установи ffmpeg (ffplay) или pulseaudio-utils (paplay)")
	}

	for _, pl := range candidates {
		if err := runPlayer(ctx, pl.bin, pl.args); err == nil {
			return nil
		} else {
			log.Warn("voice: плеер не справился, пробую следующий", "bin", pl.bin, "err", err)
		}
	}
	return errors.New("ни один плеер не воспроизвёл аудио")
}
