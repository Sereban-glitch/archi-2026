// mic.go — захват аудио со встроенного микрофона ноутбука (Linux Mint).
//
// Бэкенды записи (voice.mic.backend):
//   - arecord  — ALSA (в Mint PipeWire пробрасывается через pipewire-alsa); дефолт
//   - ffmpeg   — ffmpeg -f alsa (нужен установленный ffmpeg)
//   - parecord — PulseAudio/PipeWire-pulse (пакет pulseaudio-utils)
//
// Стиль управления — push-to-talk в терминале: запись стартует сразу,
// Enter останавливает, Ctrl+C отменяет, protect-таймер обрывает забытую запись.
// Рекордер запускается в отдельной процессной группе и получает SIGINT группе —
// WAV-заголовок дописывается корректно (arecord/ffmpeg финализируют файл).
package voice

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/archi/archi/internal/config"
)

// RecordInteractive записывает речь с микрофона до нажатия Enter
// (или до ctx-отмены / protect-таймера) и возвращает WAV 16 кГц моно.
func RecordInteractive(ctx context.Context, cfg config.VoiceMicConfig, log *slog.Logger) ([]byte, error) {
	backend, err := resolveBackend(cfg.Backend)
	if err != nil {
		return nil, err
	}
	rate := cfg.SampleRate
	if rate == 0 {
		rate = 16000
	}
	device := cfg.Device
	if device == "" {
		device = "default"
	}
	maxDur := cfg.MaxDuration.Duration
	if maxDur <= 0 {
		maxDur = 120 * time.Second
	}

	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("archi-mic-%d.wav", time.Now().UnixNano()))

	cmd, err := recorderCommand(backend, device, rate, tmp)
	if err != nil {
		return nil, err
	}
	// Своя процессная группа: наш SIGINT от Ctrl+C не должен рвать
	// рекордера раньше, чем мы сами его аккуратно остановим.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("не запустить %s: %w", backend, err)
	}

	fmt.Println("🎤 Говорите… (Enter — закончить запись, Ctrl+C — отмена)")

	// Enter в отдельной горутине: терминал может отдать его с задержкой.
	enterCh := make(chan struct{}, 1)
	go func() {
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		select {
		case enterCh <- struct{}{}:
		default:
		}
	}()

	select {
	case <-enterCh:
		// пользователь закончил
	case <-time.After(maxDur):
		fmt.Println("⏱ Достигнут максимум длительности записи, останавливаю…")
	case <-ctx.Done():
		killGroup(cmd)
		return nil, errors.New("запись отменена")
	}

	// Мягкая остановка: SIGINT группе → рекордер финализирует WAV.
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		log.Warn("voice: рекордер не завершился после SIGINT — SIGKILL")
		killGroup(cmd)
		<-done
	}

	data, err := os.ReadFile(tmp)
	_ = os.Remove(tmp)
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		return nil, fmt.Errorf("чтение записи: %w%s", err, suffixMsg(clipB([]byte(msg), 300)))
	}
	// Пустая WAV-запись — это заголовок ~44 байта; порог 1500 Б ≈ 47 мс речи.
	if len(data) < 1500 {
		return nil, errors.New("слишком короткая или пустая запись — говорите дольше")
	}
	if s := strings.TrimSpace(stderr.String()); s != "" {
		log.Debug("voice: вывод рекордера", "backend", backend, "stderr", clipB([]byte(s), 200))
	}
	return data, nil
}

// resolveBackend выбирает бэкенд записи: явно или авто-пробелом по PATH.
func resolveBackend(cfgBackend string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(cfgBackend)) {
	case "arecord", "ffmpeg", "parecord":
		return strings.ToLower(cfgBackend), nil
	case "", "auto":
		for _, cand := range []string{"arecord", "ffmpeg", "parecord"} {
			if _, err := lookCommand(cand); err == nil {
				return cand, nil
			}
		}
		return "", errors.New("не найден ни один рекордер (arecord/ffmpeg/parecord): apt install alsa-utils")
	default:
		return "", fmt.Errorf("неизвестный voice.mic.backend %q (auto|arecord|ffmpeg|parecord)", cfgBackend)
	}
}

// recorderCommand собирает команду записи WAV в tmp.
func recorderCommand(backend, device string, rate int, tmp string) (*exec.Cmd, error) {
	switch backend {
	case "arecord":
		return exec.Command("arecord", "-q", "-f", "S16_LE",
			"-r", strconv.Itoa(rate), "-c", "1", "-D", device, tmp), nil
	case "ffmpeg":
		return exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
			"-f", "alsa", "-i", device,
			"-ar", strconv.Itoa(rate), "-ac", "1", tmp), nil
	case "parecord":
		// parecord: формат вывода задаётся расширением файла (.wav).
		return exec.Command("parecord", "--rate", strconv.Itoa(rate),
			"--channels", "1", "--file-format=wav", tmp), nil
	}
	return nil, fmt.Errorf("нет сборки команды для бэкенда %q", backend)
}

func killGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// ---- общие хелперы пакета (temp-файлы, внешние команды) ----

var lookMu sync.Mutex

// lookCommand — потокобезопасный exec.LookPath.
func lookCommand(name string) (string, error) {
	lookMu.Lock()
	defer lookMu.Unlock()
	return exec.LookPath(name)
}

// runPlayer запускает плеер и ждёт завершения.
func runPlayer(ctx context.Context, bin string, args []string) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w%s", filepath.Base(bin), err, suffixMsg(clipB(stderr.Bytes(), 200)))
	}
	return nil
}

// writeTemp — атомарная запись во временный файл с заданным расширением.
func writeTemp(data []byte, ext string) (string, error) {
	if ext == "" {
		ext = ".oga"
	}
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	f, err := os.CreateTemp("", "archi-play-*"+ext)
	if err != nil {
		return "", err
	}
	name := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(name)
		return "", err
	}
	return name, f.Close()
}

func removeTemp(name string) { _ = os.Remove(name) }
