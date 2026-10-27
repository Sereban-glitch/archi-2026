package voice

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/archi/archi/internal/config"
)

// Флаги Piper должны собираться из конфига и форматироваться без экспоненты.
func TestPiperTuningArgs(t *testing.T) {
	args := piperTuningArgs(config.PiperConfig{
		NoiseScale: 0.333, NoiseW: 0.6, LengthScale: 0.95,
	})
	want := []string{"--noise_scale", "0.333", "--noise_w", "0.6", "--length_scale", "0.95"}
	if len(args) != len(want) {
		t.Fatalf("piperTuningArgs: %v, хочу %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("piperTuningArgs[%d] = %q, хочу %q", i, args[i], want[i])
		}
	}
	// нулевые параметры (конфиг выключен) — флагов нет
	if got := piperTuningArgs(config.PiperConfig{}); len(got) != 0 {
		t.Errorf("нулевые параметры должны давать пустые флаги, получили %v", got)
	}
}

// Мок command-провайдера: скрипт-обёртка читает stdin, пишет аудио-байты в stdout.
func TestSynthesizeCommandMock(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-tts.sh")
	script := "#!/bin/sh\ncat > /dev/null\nprintf 'FAKE_MP3_BYTES'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.VoiceTTSConfig{
		Enabled:  true,
		Provider: "command",
		Format:   "mp3",
		Command:  config.TTSCommandConfig{Bin: bin},
	}
	s := NewSynthesizer(cfg, testLogger())
	out, err := s.Synthesize(context.Background(), "привет, это тест 2.5 GiB")
	if err != nil {
		t.Fatalf("command TTS: %v", err)
	}
	if string(out) != "FAKE_MP3_BYTES" {
		t.Fatalf("command TTS вернул %q", out)
	}

	// Ошибка процесса = внятная ошибка
	cfg.Command.Bin = filepath.Join(dir, "no-such-bin")
	s2 := NewSynthesizer(cfg, testLogger())
	if _, err := s2.Synthesize(context.Background(), "тест"); err == nil {
		t.Fatal("хочу ошибку при мёртвом bin")
	}
}
