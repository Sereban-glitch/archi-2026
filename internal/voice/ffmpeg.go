// Package voice — голосовой интерфейс Archi: STT (распознавание речи через
// OpenAI-совместимый /v1/audio/transcriptions), опциональный TTS и захват
// микрофона ноутбука.
//
// Архитектурное решение (2026): никакого CGO и никаких тяжёлых Python-монолитов.
// Go-биндинги whisper.cpp требуют CGO и ломают статическую сборку CGO_ENABLED=0,
// поэтому локальный Whisper подключается как отдельный процесс-сервер с
// OpenAI-совместимым HTTP API (whisper.cpp server / Speaches / LocalAI),
// а облачные провайдеры (Groq whisper-large-v3-turbo, OpenAI whisper-1)
// говорят на том же протоколе. Один клиент — любой провайдер.
package voice

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

var (
	ffmpegOnce sync.Once
	ffmpegBin_ string
)

// ffmpegBinary возвращает путь к ffmpeg или "" если не установлен (кэш).
func ffmpegBinary() string {
	ffmpegOnce.Do(func() {
		if p, err := exec.LookPath("ffmpeg"); err == nil {
			ffmpegBin_ = p
		}
	})
	return ffmpegBin_
}

// FFmpegAvailable — есть ли ffmpeg в PATH (для диагностики и авто-детекта).
func FFmpegAvailable() bool { return ffmpegBinary() != "" }

// ConvertToWAV16k перекодирует произвольное аудио (ogg/opus, mp3, m4a…) в
// WAV 16 кГц моно — формат, который гарантированно принимает whisper.cpp.
// Вызывает внешний ffmpeg: тянуть кодеки внутрь бинарника (CGO) нельзя.
func ConvertToWAV16k(ctx context.Context, in []byte, filename string) ([]byte, error) {
	bin := ffmpegBinary()
	if bin == "" {
		return nil, fmt.Errorf("ffmpeg не найден в PATH (apt install ffmpeg)")
	}
	ext := strings.ToLower(filepath.Ext(filename))
	if ext == "" {
		ext = ".bin"
	}
	dir, err := os.MkdirTemp("", "archi-conv-*")
	if err != nil {
		return nil, fmt.Errorf("tmp: %w", err)
	}
	defer os.RemoveAll(dir)

	src := filepath.Join(dir, "in"+ext)
	dst := filepath.Join(dir, "out.wav")
	if err := os.WriteFile(src, in, 0o600); err != nil {
		return nil, fmt.Errorf("tmp write: %w", err)
	}

	cmd := exec.CommandContext(ctx, bin,
		"-hide_banner", "-loglevel", "error", "-y",
		"-i", src, "-ar", "16000", "-ac", "1", "-f", "wav", dst)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		return nil, fmt.Errorf("ffmpeg: %w (%s)", err, msg)
	}
	return os.ReadFile(dst)
}

// isLocalURL — указывает ли base_url на этот же хост (localhost/127.0.0.1/::1).
// Локальные серверы whisper.cpp обычно хотят WAV 16 кГц, облака жуют ogg/opus.
func isLocalURL(baseURL string) bool {
	host := baseURL
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	if i := strings.LastIndex(host, ":"); i > strings.LastIndex(host, "]") { // отрезаем порт
		host = host[:i]
	}
	host = strings.Trim(host, "[]")
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0":
		return true
	}
	return false
}

// safeName вырезает из имени файла всё, кроме букв/цифр/точек/дефисов,
// чтобы не подсунуть multipart-серверу путь или перевод строки.
func safeName(name string) string {
	name = filepath.Base(name)
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	if out == "" || strings.Trim(out, ".") == "" {
		return "audio.bin"
	}
	if len(out) > 64 {
		out = out[len(out)-64:] // расширение важнее префикса
	}
	return out
}

// suffixMsg — компактное дополнение к ошибке из stderr внешней утилиты.
func suffixMsg(msg string) string {
	if msg == "" {
		return ""
	}
	return " (" + msg + ")"
}
