package telegram

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/archi/archi/internal/config"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestBotVoiceRoundTrip — мок Bot API: getFile → скачивание файла → sendVoice.
// Проверяет весь файловый контур бота без выхода в интернет.
func TestBotVoiceRoundTrip(t *testing.T) {
	const fakeOgg = "\x4f\x67\x67\x53fake-opus-payload"

	mux := http.NewServeMux()
	// 1) getFile: file_id → file_path (бот присылает JSON, как и всем методам Bot API)
	mux.HandleFunc("/botTEST/getFile", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			FileID string `json:"file_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("getFile: битый JSON: %v", err)
		}
		if req.FileID != "f1" {
			t.Errorf("file_id = %q, хочу f1", req.FileID)
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"file_id":"f1","file_path":"voice/file_42.oga"}}`))
	})
	// 2) скачивание бинарника
	mux.HandleFunc("/file/botTEST/voice/file_42.oga", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("метод = %q, хочу GET", r.Method)
		}
		_, _ = w.Write([]byte(fakeOgg))
	})
	// 3) sendVoice: multipart с полем voice
	mux.HandleFunc("/botTEST/sendVoice", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			t.Errorf("sendVoice должен быть multipart, получил %q", r.Header.Get("Content-Type"))
		}
		file, header, err := r.FormFile("voice")
		if err != nil {
			t.Errorf("FormFile(voice): %v", err)
		} else {
			defer file.Close()
			data, _ := io.ReadAll(file)
			if string(data) != fakeOgg {
				t.Errorf("голосовые байты не совпали: %d байт", len(data))
			}
			if header.Filename != "voice.ogg" {
				t.Errorf("filename = %q, хочу voice.ogg", header.Filename)
			}
		}
		if r.FormValue("chat_id") != "100" {
			t.Errorf("chat_id = %q, хочу 100", r.FormValue("chat_id"))
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":7}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := config.TelegramConfig{APIBase: srv.URL, Token: "TEST"}
	bot := New(cfg, testLogger())

	// Скачивание вложения.
	data, err := bot.DownloadAttachment(context.Background(),
		&VoiceAttachment{FileID: "f1", FileName: "voice.ogg", Kind: "voice"}, 1<<20)
	if err != nil {
		t.Fatalf("DownloadAttachment: %v", err)
	}
	if string(data) != fakeOgg {
		t.Fatalf("скачанные байты не совпали: %q", data)
	}

	// Отправка голосового.
	if err := bot.SendVoice(context.Background(), 100, []byte(fakeOgg), 3); err != nil {
		t.Fatalf("SendVoice: %v", err)
	}
}

// TestVoiceAttachmentOf — нормализация voice/audio полей сообщения.
func TestVoiceAttachmentOf(t *testing.T) {
	if got := voiceAttachmentOf(nil); got != nil {
		t.Error("nil-сообщение не должно давать вложение")
	}
	if got := voiceAttachmentOf(&TgMessage{Text: "привет"}); got != nil {
		t.Error("текстовое сообщение не должно давать вложение")
	}
	m := &TgMessage{Voice: &TgVoice{FileID: "v1", Duration: 5}}
	got := voiceAttachmentOf(m)
	if got == nil || got.FileID != "v1" || got.FileName != "voice.ogg" || got.Kind != "voice" {
		t.Errorf("voice: %+v", got)
	}
	a := &TgMessage{Audio: &TgAudio{FileID: "a1", FileName: "note.mp3"}}
	got = voiceAttachmentOf(a)
	if got == nil || got.FileID != "a1" || got.FileName != "note.mp3" || got.Kind != "audio" {
		t.Errorf("audio: %+v", got)
	}
	// audio без имени — фолбэк на audio.mp3
	b := &TgMessage{Audio: &TgAudio{FileID: "a2"}}
	got = voiceAttachmentOf(b)
	if got == nil || got.FileName != "audio.mp3" {
		t.Errorf("audio без имени: %+v", got)
	}
}

// TestSplitMessageUTF8 — нарезка длинного текста не рвёт UTF-8 руны.
func TestSplitMessageUTF8(t *testing.T) {
	long := strings.Repeat("привет世界", 900) // 11 рун * 900 > 4000 байт
	parts := splitMessage(long, 4000)
	if len(parts) < 2 {
		t.Fatalf("ожидалась нарезка, частей: %d", len(parts))
	}
	var sb strings.Builder
	for _, p := range parts {
		sb.WriteString(p)
	}
	if sb.String() != long {
		t.Error("склейка частей не равна оригиналу")
	}
	for _, p := range parts {
		if len(p) > 4000+6 { // запас на доводку руны
			t.Errorf("часть длиннее лимита: %d", len(p))
		}
	}
}
