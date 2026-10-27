// wake_dtw.go — офлайн wake-word на шаблонах: MFCC + DTW (чистый Go).
//
// Зачем: движок stt завязывает wake-word на распознавание (облако/сервер) —
// нет сети или ключа, и «Арчи» не слышат вообще. Этот движок ловит wake-word
// локально, по шаблонам, записанным голосом владельца (`archi ear --train-wake`):
//
//	MFCC (25 мс окно / 10 мс сдвиг, 26 mel-фильтров, 13 кепстр) → CMVN → DTW
//	со скользящим окном по фразе + порогом из кросс-валидации образцов.
//
// Спикер-зависимо (учится голос владельца на его же микрофоне) — для личного
// ноутбука это плюс: чужие голоса и ТВ не будят агента.
//
// Это классика жанра keyword-споттинга до нейросетей: честно работает на 2 ядрах
// без единой модели в памяти. Следующий уровень качества — сайдкар с настоящей
// моделью (openwakeword.com / livekit-wakeword, формат ONNX) через wake_engine: http.
package voice

import (
	"encoding/json"
	"fmt"
	"math"
	"math/cmplx"
	"os"
	"path/filepath"
	"time"
)

// ---- FFT (radix-2, in-place) ----

// fft — БПФ с прореживанием по времени; len(x) обязан быть степенью двойки.
func fft(x []complex128) {
	n := len(x)
	if n&(n-1) != 0 {
		panic("fft: длина не степень двойки")
	}
	// перестановка с реверсом битов.
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j |= bit
		if i < j {
			x[i], x[j] = x[j], x[i]
		}
	}
	for length := 2; length <= n; length <<= 1 {
		ang := -2 * math.Pi / float64(length)
		wlen := cmplx.Exp(complex(ang, 0))
		for i := 0; i < n; i += length {
			w := complex(1, 0)
			for k := 0; k < length/2; k++ {
				u := x[i+k]
				v := x[i+k+length/2] * w
				x[i+k] = u + v
				x[i+k+length/2] = u - v
				w *= wlen
			}
		}
	}
}

// ---- MFCC ----

const (
	mfccFrameLen = 400 // 25 мс @ 16 кГц
	mfccShift    = 160 // 10 мс @ 16 кГц
	mfccNFFT     = 512
	mfccNMel     = 26
	mfccCEP      = 12 // кепстра c1..c12 (c0 — чистая энергия, шумит — не берём)
	mfccEps      = 1e-10
	mfccScale    = 2.0 // типичный размах log-mel речи: приводим расстояния к переносимым порогам
)

// mfcc превращает сырой PCM s16le (моно, rate) в последовательность
// нормированных (CMVN) кепстральных векторов — по кадрам 25/10 мс.
func mfcc(pcm []byte, rate int) [][]float64 {
	if rate <= 0 {
		rate = 16000
	}
	samples := make([]float64, len(pcm)/2)
	for i := range samples {
		s := int16(uint16(pcm[2*i]) | uint16(pcm[2*i+1])<<8)
		samples[i] = float64(s) / 32768.0
	}
	if len(samples) < mfccFrameLen {
		return nil // короче одного кадра — нечего сравнивать
	}

	fmax := float64(rate) / 2
	if fmax > 7600 {
		fmax = 7600
	}
	// Окно Хэмминга.
	hamming := make([]float64, mfccFrameLen)
	for i := range hamming {
		hamming[i] = 0.54 - 0.46*math.Cos(2*math.Pi*float64(i)/float64(mfccFrameLen-1))
	}
	// Треугольные mel-фильтры (границы в бинах спектра).
	bin := func(hz float64) int {
		return int(math.Round(hz / float64(rate) * float64(mfccNFFT)))
	}
	mel := func(hz float64) float64 { return 2595 * math.Log10(1+hz/700) }
	inv := func(m float64) float64 { return 700 * (math.Pow(10, m/2595) - 1) }
	mLow, mHigh := mel(60), mel(fmax)
	edges := make([]int, mfccNMel+2)
	for i := 0; i < mfccNMel+2; i++ {
		edges[i] = bin(inv(mLow + (mHigh-mLow)*float64(i)/float64(mfccNMel+1)))
	}

	frames := (len(samples) - mfccFrameLen) / mfccShift
	out := make([][]float64, 0, frames)
	buf := make([]complex128, mfccNFFT)
	spec := make([]float64, mfccNFFT/2+1)
	melE := make([]float64, mfccNMel)
	cep := make([]float64, mfccCEP)

	for f := 0; f < frames; f++ {
		off := f * mfccShift
		for i := 0; i < mfccNFFT; i++ {
			if i < mfccFrameLen {
				buf[i] = complex(samples[off+i]*hamming[i], 0)
			} else {
				buf[i] = 0
			}
		}
		fft(buf)
		for i := range spec {
			spec[i] = real(buf[i])*real(buf[i]) + imag(buf[i])*imag(buf[i])
		}
		for m := 0; m < mfccNMel; m++ {
			lo, mid, hi := edges[m], edges[m+1], edges[m+2]
			if mid <= lo || hi <= mid {
				melE[m] = math.Log(mfccEps)
				continue
			}
			var e float64
			for k := lo + 1; k < mid && k < len(spec); k++ {
				e += spec[k] * float64(k-lo) / float64(mid-lo)
			}
			for k := mid; k < hi && k < len(spec); k++ {
				e += spec[k] * float64(hi-k) / float64(hi-mid)
			}
			melE[m] = math.Log(e + mfccEps)
		}
		// DCT-II → кепстр c1..c12 (c0 — энергия — отброшен: он шумит и не дискриминирует).
		for c := 0; c < mfccCEP; c++ {
			var s float64
			for m := 0; m < mfccNMel; m++ {
				s += melE[m] * math.Cos(math.Pi*float64(c+1)*(float64(m)+0.5)/float64(mfccNMel))
			}
			cep[c] = s
		}
		vec := make([]float64, mfccCEP)
		for c, v := range cep {
			vec[c] = v / mfccScale
		}
		out = append(out, vec)
	}
	return out
}

// Заметка о нормализации: кепстра c1..c12 УЖЕ амплитудно-инвариантна —
// умножение сигнала на α добавляет log α КО ВСЕМ mel-бинам, а DCT-базисы
// для c≥1 имеют нулевую сумму → коэффициенты не меняются (меняется только c0,
// который мы отбросили). Поэтому CMVN по фразе НЕ применяется: проверено
// тестами — вычитание среднего по фразе только ЗАГРЯЗНЯЕТ скользящее окно
// средним от обрамляющего шума. Фиксированный масштаб mfccScale держит
// расстояния в переносимом диапазоне порогов.

// ---- DTW ----

// dtwDistance — DTW двух последовательностей векторов с лентой Сакое-Чибы.
// Возвращает среднюю стоимость кадра на пути (нормировка на длину пути) —
// это и есть «расстояние» для порога.
func dtwDistance(a, b [][]float64) float64 {
	n, m := len(a), len(b)
	if n == 0 || m == 0 {
		return math.Inf(1)
	}
	const inf = math.MaxFloat64 / 4
	// Радиус ленты: допускаем временнУю деформацию до ~35%.
	band := 0.35*float64(n) + 2
	// DP по стоимости и длине пути.
	cost := make([]float64, m+1)
	length := make([]int, m+1)
	prevCost := make([]float64, m+1)
	prevLen := make([]int, m+1)
	for j := 0; j <= m; j++ {
		cost[j], prevCost[j] = inf, inf
	}
	prevCost[0], prevLen[0] = 0, 0

	frameCost := func(i, j int) float64 {
		var s float64
		for c, v := range a[i] {
			d := v - b[j][c]
			s += d * d
		}
		return math.Sqrt(s)
	}
	for i := 1; i <= n; i++ {
		cost[0] = inf
		lo := int(math.Max(1, float64(i)*float64(m)/float64(n)-band))
		hi := int(math.Min(float64(m), float64(i)*float64(m)/float64(n)+band))
		for j := 1; j <= m; j++ {
			if j < lo || j > hi {
				cost[j] = inf
				length[j] = 0
				continue
			}
			// Классические три перехода.
			best, bl := prevCost[j], prevLen[j] // вверх (i-1, j)
			if prevCost[j-1] < best {           // диагональ
				best, bl = prevCost[j-1], prevLen[j-1]
			}
			if cost[j-1] < best { // влево (i, j-1)
				best, bl = cost[j-1], length[j-1]
			}
			if best >= inf {
				cost[j] = inf
				length[j] = 0
				continue
			}
			cost[j] = best + frameCost(i-1, j-1)
			length[j] = bl + 1
		}
		prevCost, cost = cost, prevCost
		prevLen, length = length, prevLen
	}
	if prevCost[m] >= inf || prevLen[m] == 0 {
		return math.Inf(1)
	}
	return prevCost[m] / float64(prevLen[m])
}

// matchTemplate ищет шаблон внутри фразы скользящим окном.
// Возвращает минимальное нормированное расстояние.
func matchTemplate(utt, tmpl [][]float64) float64 {
	if len(utt) == 0 || len(tmpl) == 0 {
		return math.Inf(1)
	}
	best := math.Inf(1)
	// Окно чуть длиннее шаблона (речь растягивается ±25%), шаг 3 кадра (30 мс).
	win := int(float64(len(tmpl)) * 1.25)
	if win < 5 {
		win = 5
	}
	for start := 0; start+5 <= len(utt); start += 3 {
		end := start + win
		if end > len(utt) {
			end = len(utt)
		}
		d := dtwDistance(tmpl, utt[start:end])
		if d < best {
			best = d
		}
	}
	// Фраза короче шаблона — сравниваем целиком (полное выравнивание).
	if len(utt) < win {
		if d := dtwDistance(tmpl, utt); d < best {
			best = d
		}
	}
	return best
}

// ---- хранение шаблонов ----

// WakeTemplates — выученные образцы wake-word (после `archi ear --train-wake`).
type WakeTemplates struct {
	Version   int           `json:"version"` // 1
	TrainedAt time.Time     `json:"trained_at"`
	WakeWord  string        `json:"wake_word"`
	Threshold float64       `json:"threshold"`
	Samples   [][][]float64 `json:"samples"` // MFCC-кадры каждого образца
}

const wakeTemplatesFile = "wake-templates.json"

// LoadWakeTemplates читает шаблоны из каталога данных.
func LoadWakeTemplates(dataDir string) (*WakeTemplates, error) {
	raw, err := os.ReadFile(filepath.Join(dataDir, wakeTemplatesFile))
	if err != nil {
		return nil, err
	}
	var t WakeTemplates
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("wake: шаблоны повреждены: %w", err)
	}
	if len(t.Samples) == 0 {
		return nil, fmt.Errorf("wake: в шаблонах нет образцов")
	}
	return &t, nil
}

// SaveWakeTemplates атомарно пишет шаблоны (tmp + rename).
func SaveWakeTemplates(dataDir string, t *WakeTemplates) error {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	raw, err := json.Marshal(t)
	if err != nil {
		return err
	}
	path := filepath.Join(dataDir, wakeTemplatesFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// MatchWake проверяет фразу против всех шаблонов, возвращает
// (минимальное расстояние, совпало ли с порогом).
func (t *WakeTemplates) MatchWake(pcm []byte, rate int) (float64, bool) {
	utt := mfcc(pcm, rate)
	if len(utt) == 0 {
		return math.Inf(1), false
	}
	best := math.Inf(1)
	for _, s := range t.Samples {
		if d := matchTemplate(utt, s); d < best {
			best = d
		}
	}
	return best, best <= t.Threshold
}
