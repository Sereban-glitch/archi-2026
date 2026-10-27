// wake_dtw_test.go — тесты офлайн wake-word: MFCC, DTW, скользящее окно,
// хранение шаблонов. Синтетика: синусы/чирпы вместо речи — стабильные сигналы.
package voice

import (
	"encoding/binary"
	"math"
	"math/rand"
	"path/filepath"
	"testing"
	"time"
)

// synthTone — синус частоты f Гц длительностью ms (s16le моно, rate).
func synthTone(f float64, ms, rate int, amp float64) []byte {
	n := rate * ms / 1000
	out := make([]byte, n*2)
	for i := 0; i < n; i++ {
		v := amp * math.Sin(2*math.Pi*f*float64(i)/float64(rate))
		binary.LittleEndian.PutUint16(out[i*2:], uint16(int16(v)))
	}
	return out
}

// synthChirp — частотная дорожка f0→f1 (ближе к слогу, чем чистый тон).
func synthChirp(f0, f1 float64, ms, rate int, amp float64) []byte {
	n := rate * ms / 1000
	out := make([]byte, n*2)
	phase := 0.0
	for i := 0; i < n; i++ {
		f := f0 + (f1-f0)*float64(i)/float64(n)
		phase += 2 * math.Pi * f / float64(rate)
		v := amp * math.Sin(phase)
		binary.LittleEndian.PutUint16(out[i*2:], uint16(int16(v)))
	}
	return out
}

// synthNoise — псевдослучайный шум (детерминированный рандом).
func synthNoise(ms, rate int, amp float64) []byte {
	rnd := rand.New(rand.NewSource(42))
	n := rate * ms / 1000
	out := make([]byte, n*2)
	for i := 0; i < n; i++ {
		v := amp * (rnd.Float64()*2 - 1)
		binary.LittleEndian.PutUint16(out[i*2:], uint16(int16(v)))
	}
	return out
}

func pcmConcat(parts ...[]byte) []byte {
	var total int
	for _, p := range parts {
		total += len(p)
	}
	out := make([]byte, 0, total)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func TestMFCCFrameCountAndShape(t *testing.T) {
	pcm := synthTone(1000, 1000, 16000, 12000) // 1 c тона 1 кГц
	frames := mfcc(pcm, 16000)
	// (16000 - 400) / 160 = 97.5 → 97 кадров.
	if len(frames) < 90 || len(frames) > 99 {
		t.Fatalf("кадров %d, хочу ~97", len(frames))
	}
	if dim := len(frames[0]); dim != mfccCEP {
		t.Fatalf("размерность %d, хочу %d", dim, mfccCEP)
	}
	// CMVN: у константного сигнала все кадры после нормировки нулевые.
	sil := mfcc(synthTone(0, 300, 16000, 0), 16000)
	for _, fr := range sil {
		for _, v := range fr {
			if math.Abs(v) > 1e-6 {
				t.Fatalf("тишина после CMVN не ноль: %v", v)
			}
		}
	}
}

func TestDTWIdenticalNearZero(t *testing.T) {
	a := mfcc(synthChirp(500, 900, 500, 16000, 10000), 16000)
	b := mfcc(synthChirp(500, 900, 500, 16000, 10000), 16000)
	if d := dtwDistance(a, b); d > 0.5 {
		t.Fatalf("DTW(одинаковые) = %.3f, хочу ~0", d)
	} else {
		t.Logf("DTW(одинаковые) = %.3f", d)
	}
}

func TestDTWDifferentSignalsFar(t *testing.T) {
	a := mfcc(synthChirp(500, 900, 500, 16000, 10000), 16000)   // «слово»
	b := mfcc(synthChirp(2500, 3200, 500, 16000, 10000), 16000) // другое «слово»
	same := dtwDistance(a, a)
	diff := dtwDistance(a, b)
	t.Logf("DTW: одинаковые=%.3f разные=%.3f", same, diff)
	if diff < same+1.0 {
		t.Fatalf("DTW(разные)=%.2f недостаточно больше DTW(одинаковые)=%.2f", diff, same)
	}
}

func TestMatchTemplateSlidingWindow(t *testing.T) {
	// Шаблон — «wake-word»: чирп 0.5 с.
	tmpl := mfcc(synthChirp(400, 850, 500, 16000, 10000), 16000)
	// Фраза — чирп внутри обрамления из шума (как речь в тишине).
	utt := pcmConcat(synthNoise(300, 16000, 400), synthChirp(400, 850, 520, 16000, 10000), synthNoise(400, 16000, 400))
	uttF := mfcc(utt, 16000)

	in := matchTemplate(uttF, tmpl)
	t.Logf("matchTemplate(чирп внутри фразы) = %.3f", in)
	// Совсем другой сигнал — шум — не должен матчиться под тем же порогом.
	noiseF := mfcc(synthNoise(1200, 16000, 5000), 16000)
	noise := matchTemplate(noiseF, tmpl)
	t.Logf("matchTemplate(шум) = %.3f", noise)
	// Разное «слово» (чирп в другом частотном диапазоне).
	otherF := mfcc(pcmConcat(synthNoise(200, 16000, 400), synthChirp(2600, 3400, 520, 16000, 10000), synthNoise(300, 16000, 400)), 16000)
	other := matchTemplate(otherF, tmpl)
	t.Logf("matchTemplate(другое слово) = %.3f", other)

	if in > 6.0 {
		t.Fatalf("matchTemplate(чирп внутри фразы) = %.2f, хочу ≤ 6.0 (рабочий порог)", in)
	}
	if noise <= in+3.0 {
		t.Fatalf("шум (%.2f) подозрительно близко к своему сигналу (%.2f)", noise, in)
	}
	if other <= in+3.0 {
		t.Fatalf("чужое слово (%.2f) подозрительно близко к своему (%.2f)", other, in)
	}
}

func TestWakeTemplatesRoundTripAndMatch(t *testing.T) {
	dir := t.TempDir()
	tpl := &WakeTemplates{
		Version:   1,
		TrainedAt: time.Now(),
		WakeWord:  "арчи",
		Threshold: 6.5,
		Samples: [][][]float64{
			mfcc(synthChirp(400, 850, 500, 16000, 10000), 16000),
			mfcc(synthChirp(420, 880, 550, 16000, 10000), 16000),
			mfcc(synthChirp(390, 830, 480, 16000, 10000), 16000),
		},
	}
	if err := SaveWakeTemplates(dir, tpl); err != nil {
		t.Fatalf("сохранение: %v", err)
	}
	got, err := LoadWakeTemplates(dir)
	if err != nil {
		t.Fatalf("чтение: %v", err)
	}
	if len(got.Samples) != 3 || got.WakeWord != "арчи" {
		t.Fatalf("после круга: samples=%d word=%q", len(got.Samples), got.WakeWord)
	}
	// Тот же сигнал, что в шаблонах → совпадение.
	d, m := got.MatchWake(synthChirp(410, 860, 510, 16000, 11000), 16000)
	t.Logf("MatchWake(родной) = %.3f (threshold %.2f)", d, got.Threshold)
	if !m {
		t.Fatal("MatchWake не узнал родной сигнал")
	}
	// Посторонний сигнал (высокочастотный чирп) — не совпал.
	d2, m2 := got.MatchWake(pcmConcat(synthNoise(200, 16000, 300), synthChirp(2600, 3400, 600, 16000, 11000)), 16000)
	t.Logf("MatchWake(чужой) = %.3f", d2)
	if m2 {
		t.Fatal("MatchWake поймал чужой сигнал (ложняк)")
	}
	// Пустой каталог → внятная ошибка.
	if _, err := LoadWakeTemplates(filepath.Join(dir, "nope")); err == nil {
		t.Fatal("LoadWakeTemplates должен падать без файла")
	}
}
