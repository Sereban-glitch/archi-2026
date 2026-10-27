package voice

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/archi/archi/internal/config"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestSafeName(t *testing.T) {
	cases := map[string]string{
		"voice.oga":           "voice.oga",
		"../../../etc/passwd": "passwd",
		"my voice note.mp3":   "my_voice_note.mp3",
		"":                    "audio.bin",
		"..":                  "audio.bin",
		"файл.ogg":            "____.ogg",
		"a\nb.ogg":            "a_b.ogg",
	}
	for in, want := range cases {
		if got := safeName(in); got != want {
			t.Errorf("safeName(%q) = %q, хочу %q", in, got, want)
		}
	}
}

func TestIsLocalURL(t *testing.T) {
	cases := map[string]bool{
		"http://127.0.0.1:18080":         true,
		"http://localhost:9000/v1":       true,
		"https://api.groq.com/openai/v1": false,
		"http://192.168.1.5:9000":        false,
		"https://api.openai.com/v1":      false,
	}
	for in, want := range cases {
		if got := isLocalURL(in); got != want {
			t.Errorf("isLocalURL(%q) = %v, хочу %v", in, got, want)
		}
	}
}

func TestPlainText(t *testing.T) {
	in := "*жирный* и `код` с #заголовком\n> цитата _и_ **вложенный**"
	want := "жирный и код с заголовком\n цитата и вложенный"
	if got := PlainText(in); got != want {
		t.Errorf("PlainText = %q, хочу %q", got, want)
	}
}

// TestTranscriberMock — полный цикл STT против мок-сервера, говорящего
// на OpenAI-совместимом протоколе /audio/transcriptions.
func TestTranscriberMock(t *testing.T) {
	var gotModel, gotLanguage, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/transcriptions" {
			t.Errorf("путь = %q, хочу /audio/transcriptions", r.URL.Path)
		}
		gotModel = r.FormValue("model")
		gotLanguage = r.FormValue("language")
		gotCT = r.Header.Get("Content-Type")
		if gotCT == "" || len(gotCT) < 20 {
			t.Errorf("Content-Type подозрительно пуст: %q", gotCT)
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("FormFile: %v", err)
		}
		defer file.Close()
		if header.Filename != "voice.ogg" {
			t.Errorf("filename = %q, хочу voice.ogg", header.Filename)
		}
		data, _ := io.ReadAll(file)
		if len(data) == 0 {
			t.Error("пустое аудио в multipart")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"text": "проверь память на сервере"})
	}))
	defer srv.Close()

	cfg := config.VoiceSTTConfig{
		Enabled:  true,
		BaseURL:  srv.URL,
		Model:    "whisper-large-v3-turbo",
		Language: "ru",
		Convert:  "never", // мок — без реального ffmpeg
		Timeout:  config.Duration{Duration: 5 * time.Second},
	}
	tr := NewTranscriber(cfg, testLogger())
	text, err := tr.Transcribe(context.Background(), []byte("fake-ogg-bytes"), "voice.ogg")
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if text != "проверь память на сервере" {
		t.Errorf("транскрипт = %q", text)
	}
	if gotModel != "whisper-large-v3-turbo" {
		t.Errorf("model = %q", gotModel)
	}
	if gotLanguage != "ru" {
		t.Errorf("language = %q", gotLanguage)
	}
}

// TestTranscriberHTTPError — 4xx от провайдера не должен превращаться в ретрай-цикл.
func TestTranscriberHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"invalid api key"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	cfg := config.VoiceSTTConfig{
		Enabled: true,
		BaseURL: srv.URL,
		Model:   "whisper-1",
		Convert: "never",
		Timeout: config.Duration{Duration: 2 * time.Second},
	}
	tr := NewTranscriber(cfg, testLogger())
	_, err := tr.Transcribe(context.Background(), []byte("fake"), "voice.ogg")
	if err == nil {
		t.Fatal("ожидалась ошибка 401")
	}
	if want := "401"; !contains(err.Error(), want) {
		t.Errorf("ошибка должна содержать %q: %v", want, err)
	}
}

// TestTranscriberEmptyAudio — граница: пустой вход.
func TestTranscriberEmptyAudio(t *testing.T) {
	tr := NewTranscriber(config.VoiceSTTConfig{Convert: "never"}, testLogger())
	if _, err := tr.Transcribe(context.Background(), nil, "voice.ogg"); err == nil {
		t.Fatal("пустое аудио должно давать ошибку")
	}
}

// TestConvertToWAV16kIntegration — полный цикл конверсии: OGG/Opus (формат
// голосовых Telegram) → WAV 16 кГц моно. Пропускается без ffmpeg.
func TestConvertToWAV16kIntegration(t *testing.T) {
	if !FFmpegAvailable() {
		t.Skip("ffmpeg не установлен — проверю на ноутбуке")
	}
	dir := t.TempDir()
	src := dir + "/voice.ogg"
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1", "-c:a", "libopus", "-b:a", "24k", src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg не создал тестовый ogg: %v (%s)", err, out)
	}
	ogg, err := os.ReadFile(src)
	if err != nil || len(ogg) < 100 {
		t.Fatalf("тестовый ogg не читается: %v, %d байт", err, len(ogg))
	}

	wav, err := ConvertToWAV16k(context.Background(), ogg, "voice.ogg")
	if err != nil {
		t.Fatalf("ConvertToWAV16k: %v", err)
	}
	// Проверка RIFF-заголовка и параметров формата.
	if len(wav) < 44 || string(wav[:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		t.Fatalf("это не WAV: заголовок %q", string(wav[:12]))
	}
	rate := int(wav[24]) | int(wav[25])<<8 | int(wav[26])<<16 | int(wav[27])<<24
	if rate != 16000 {
		t.Errorf("sample rate = %d, хочу 16000", rate)
	}
	if wav[22] != 1 { // NumChannels
		t.Errorf("каналов = %d, хочу 1 (моно)", wav[22])
	}
}

// TestTranscriberLocalConvert — полный путь локального провайдера:
// ogg/opus → ffmpeg → wav 16кГц → multipart → мок whisper.cpp на localhost.
func TestTranscriberLocalConvert(t *testing.T) {
	if !FFmpegAvailable() {
		t.Skip("ffmpeg не установлен — проверю на ноутбуке")
	}
	dir := t.TempDir()
	src := dir + "/voice.ogg"
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1", "-c:a", "libopus", "-b:a", "24k", src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v (%s)", err, out)
	}
	ogg, _ := os.ReadFile(src)

	var gotName string
	var gotFirstBytes []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Errorf("FormFile: %v", err)
			http.Error(w, "bad", 400)
			return
		}
		defer file.Close()
		gotName = header.Filename
		gotFirstBytes, _ = io.ReadAll(io.LimitReader(file, 12))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"text": "ok"})
	}))
	defer srv.Close()

	// convert=auto + localhost → должен сконвертировать в WAV и отправить .wav.
	cfg := config.VoiceSTTConfig{
		Enabled: true,
		BaseURL: srv.URL, // 127.0.0.1 → isLocalURL=true
		Model:   "whisper-1",
		Convert: "auto",
		Timeout: config.Duration{Duration: 15 * time.Second},
	}
	tr := NewTranscriber(cfg, testLogger())
	if _, err := tr.Transcribe(context.Background(), ogg, "voice.ogg"); err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if gotName != "audio.wav" {
		t.Errorf("filename = %q, хочу audio.wav (после конверсии)", gotName)
	}
	if len(gotFirstBytes) < 4 || string(gotFirstBytes[:4]) != "RIFF" {
		t.Errorf("на сервер должно уехать WAV (RIFF-заголовок), первые байты: %q", gotFirstBytes)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
