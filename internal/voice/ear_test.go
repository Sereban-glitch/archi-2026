// ear_test.go — тесты hands-free слушателя: VAD-стейт-машина, wake-word,
// WAV-заголовок. Стриминг и рекордер не тестируются — требуют железо.
package voice

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/archi/archi/internal/config"
)

// makeFrame — кадр из 480 сэмплов s16le постоянной амплитуды (30 мс @ 16 кГц).
func makeFrame(amp int16) []byte {
	const samples = 480
	out := make([]byte, samples*2)
	for i := 0; i < samples; i++ {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(amp))
	}
	return out
}

func TestRMS(t *testing.T) {
	if got := rms(makeFrame(1000)); got < 999 || got > 1001 {
		t.Fatalf("rms(1000) = %v, хочу ~1000", got)
	}
	if got := rms(makeFrame(0)); got != 0 {
		t.Fatalf("rms(0) = %v, хочу 0", got)
	}
	if got := rms(nil); got != 0 {
		t.Fatalf("rms(nil) = %v, хочу 0", got)
	}
}

// feedSeq прогоняет кадры через VAD, возвращает последнее событие.
func feedSeq(v *vad, frame []byte, n int) vadEvent {
	var last vadEvent
	for i := 0; i < n; i++ {
		v.SetLastFrame(frame)
		last = v.feed(rms(frame))
	}
	return last
}

func TestVADUtteranceFlow(t *testing.T) {
	v := newVAD(config.EarVADConfig{
		FrameMs: 30, StartFactor: 3.2, StopFactor: 1.9,
		MinSpeechMs: 320, SilenceMs: 1100, NoiseLearnS: 1,
	})
	silence := makeFrame(100) // шумовой пол
	voice := makeFrame(1500)  // речь

	// Фаза обучения (1 c тишины): событий нет.
	for i := 0; i < 40; i++ {
		v.SetLastFrame(silence)
		if ev := v.feed(rms(silence)); ev != vadNone {
			t.Fatalf("в калибровке событие %v", ev)
		}
	}
	if v.Learning() {
		t.Fatal("калибровка не закончилась")
	}
	floor := v.Floor()
	if floor < 90 || floor > 110 {
		t.Fatalf("пол шума = %v, хочу ~100", floor)
	}

	// Тишина до триггера — копится pre-roll.
	feedSeq(v, silence, 10)

	// Речь: первый кадр — старт события.
	v.SetLastFrame(voice)
	if ev := v.feed(rms(voice)); ev != vadSpeechStart {
		t.Fatalf("первый речевой кадр: %v, хочу vadSpeechStart", ev)
	}
	feedSeq(v, voice, 25) // ~750 мс речи

	// Хвост тишины → конец фразы.
	var got vadEvent
	for i := 0; i < 50; i++ {
		v.SetLastFrame(silence)
		if ev := v.feed(rms(silence)); ev == vadUtterance {
			got = vadUtterance
			break
		}
	}
	if got != vadUtterance {
		t.Fatal("фраза не завершилась по тишине")
	}
	pcm := v.takeUtterance()
	if len(pcm) == 0 {
		t.Fatal("пустая фраза")
	}
	// preroll (~400 мс) + речь (~750 мс): минимум 20 кадров данных
	if min := 20 * 960; len(pcm) < min {
		t.Fatalf("фраза подозрительно короткая: %d Б", len(pcm))
	}
	if again := v.takeUtterance(); len(again) != 0 {
		t.Fatalf("повторное takeUtterance отдало %d Б", len(again))
	}
}

func TestVADShortBurstIsNoise(t *testing.T) {
	v := newVAD(config.EarVADConfig{
		FrameMs: 30, StartFactor: 3.2, StopFactor: 1.9,
		MinSpeechMs: 320, SilenceMs: 1100, NoiseLearnS: 1,
	})
	silence := makeFrame(100)
	voice := makeFrame(1500)

	feedSeq(v, silence, 40) // калибровка
	feedSeq(v, silence, 5)  // тишина
	// Короткий всплеск ~90 мс < 320 мс — это клик, не фраза.
	v.SetLastFrame(voice)
	if ev := v.feed(rms(voice)); ev != vadSpeechStart {
		t.Fatalf("старт: %v", ev)
	}
	feedSeq(v, voice, 2)
	// Тишина: фраза «заканчивается», но речи было мало — выброс.
	var saw bool
	for i := 0; i < 50; i++ {
		v.SetLastFrame(silence)
		if ev := v.feed(rms(silence)); ev == vadUtterance {
			saw = true
		}
	}
	if saw {
		t.Fatal("клик короче min_speech_ms не должен давать фразу")
	}
	if got := v.takeUtterance(); len(got) != 0 {
		t.Fatalf("после клика буфер не пуст: %d Б", len(got))
	}
}

func TestVADMaxUtterance(t *testing.T) {
	v := newVAD(config.EarVADConfig{
		FrameMs: 30, StartFactor: 3.2, StopFactor: 1.9,
		MinSpeechMs: 320, SilenceMs: 1100, NoiseLearnS: 1,
	})
	v.setMaxUtterance(600) // 0.6 c максимум
	silence := makeFrame(100)
	voice := makeFrame(1500)

	feedSeq(v, silence, 40)
	feedSeq(v, silence, 3)
	v.SetLastFrame(voice)
	_ = v.feed(rms(voice))

	var got vadEvent
	for i := 0; i < 40; i++ { // говорим без пауз дольше лимита
		v.SetLastFrame(voice)
		if ev := v.feed(rms(voice)); ev == vadUtterance {
			got = vadUtterance
			break
		}
	}
	if got != vadUtterance {
		t.Fatal("max_utterance не обрубил бесконечную речь")
	}
}

func TestWrapWAV(t *testing.T) {
	pcm := make([]byte, 3200)
	wav := wrapWAV(pcm, 16000)
	if len(wav) != 44+3200 {
		t.Fatalf("размер = %d, хочу %d", len(wav), 44+3200)
	}
	if !bytes.Equal(wav[0:4], []byte("RIFF")) || !bytes.Equal(wav[8:12], []byte("WAVE")) {
		t.Fatal("нет RIFF/WAVE магии")
	}
	if binary.LittleEndian.Uint32(wav[24:]) != 16000 {
		t.Fatal("sample rate не 16000")
	}
	if binary.LittleEndian.Uint32(wav[40:]) != 3200 {
		t.Fatal("data size неверен")
	}
}

func TestSplitWakeWord(t *testing.T) {
	words := []string{"арчи", "archi"}
	cases := []struct {
		in     string
		wantOK bool
		want   string
	}{
		{"Арчи, проверь диск", true, "проверь диск"},
		{"арчи?", true, ""},
		{"Арчи", true, ""},
		{"ARCHI status please", true, "status please"},
		{"Archi check disk", true, "check disk"},
		{"ARCHY, status", true, "status"},
		{"Арчи, покажи статус нод", true, "покажи статус нод"},
		{"арче, проверь диск", true, "проверь диск"},  // опечатка в кириллице
		{"ар чи, проверь диск", true, "проверь диск"}, // склейка пробелом
		{"проверь диск", false, ""},
		{"арчии проверь", true, "проверь"}, // опечатка длиной 1 (dist≤1)
		{"арчи-проверь", true, "проверь"},  // дефис как разделитель
		{"архитектура хорошая", false, ""}, // длинное слово — не wake
		{"check disk please", false, ""},
		{"привет как дела", false, ""},
		{"", false, ""},
	}
	for _, c := range cases {
		rest, ok := splitWakeWord(c.in, words)
		if ok != c.wantOK || rest != c.want {
			t.Errorf("splitWakeWord(%q) = (%q, %v), хочу (%q, %v)", c.in, rest, ok, c.want, c.wantOK)
		}
	}
}

func TestEditDistance(t *testing.T) {
	if d := editDistance("арчи", "арчи"); d != 0 {
		t.Errorf("dist(арчи,арчи) = %d", d)
	}
	if d := editDistance("арчи", "арчии"); d != 1 {
		t.Errorf("dist(арчи,арчии) = %d", d)
	}
	if d := editDistance("archi", "arche"); d != 1 {
		t.Errorf("dist(archi,arche) = %d", d)
	}
}

func TestPlayExt(t *testing.T) {
	cases := map[string]string{
		"opus": ".oga", "": ".oga", "mp3": ".mp3", "wav": ".wav", "FLAC": ".flac",
	}
	for in, want := range cases {
		if got := playExt(in); got != want {
			t.Errorf("playExt(%q) = %q, хочу %q", in, got, want)
		}
	}
}
