// ear_diag.go — самодиагностика ушей и обучение wake-шаблонов.
//
//	archi ear --test        — 15-секундный прогон всей цепочки: захват → VAD →
//	                          STT → wake-совпадение → IPC, с вердиктами по шагам.
//	archi ear --train-wake  — записать 5 образцов «Арчи» голосом владельца,
//	                          построить MFCC-шаблоны, подобрать порог, сохранить.
//
// Обе команды человекочитаемы (stdout) — вывод можно прислать в чат для
// дистанционной диагностики. Запускать их нужно в том же окружении, что и
// слушатель: если ear живёт в systemd — sudo -u archi XDG_RUNTIME_DIR=… archi ear --test.
package voice

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"os"
	"strings"
	"time"

	"github.com/archi/archi/internal/config"
)

// levelBar — псевдографика громкости для живого индикатора.
func levelBar(rms, peak float64) string {
	const blocks = "▁▂▃▄▅▆▇█"
	if peak <= 0 {
		return string(blocks[0])
	}
	idx := int(rms / peak * float64(len(blocks)-1))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(blocks) {
		idx = len(blocks) - 1
	}
	return string(blocks[idx])
}

// readFrames читает кадры PCM в канал до ошибки/конца потока.
func readFrames(ctx context.Context, rec *streamRecorder, frameBytes int) <-chan []byte {
	ch := make(chan []byte, 64)
	go func() {
		defer close(ch)
		buf := make([]byte, frameBytes)
		for {
			if _, err := io.ReadFull(rec.stdout, buf); err != nil {
				if ctx.Err() == nil {
					fmt.Printf("  ⚠ поток оборвался: %v | stderr: %s\n",
						err, clipE(rec.stderr.String(), 200))
				}
				return
			}
			frame := make([]byte, len(buf))
			copy(frame, buf)
			select {
			case ch <- frame:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch
}

// RunEarTest — пошаговая проверка слуха: захват, уровень, VAD, STT, wake, IPC.
func RunEarTest(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	ear := cfg.Ear
	rate := cfg.Voice.Mic.SampleRate
	if rate == 0 {
		rate = 16000
	}
	frameMs := ear.VAD.FrameMs
	if frameMs <= 0 {
		frameMs = 30
	}
	frameBytes := rate / 1000 * frameMs * 2

	fmt.Println("═══ Диагностика ушей Archi ═══")
	backend, err := resolveBackend(cfg.Voice.Mic.Backend)
	if err != nil {
		fmt.Printf("❌ Бэкенд записи: %v (apt install alsa-utils)\n", err)
		return err
	}
	device := cfg.Voice.Mic.Device
	if device == "" {
		device = "default"
	}
	tpl := ""
	if t, err := LoadWakeTemplates(cfg.DataDir); err == nil {
		tpl = fmt.Sprintf("есть (%d образцов, порог %.2f, слово «%s»)",
			len(t.Samples), t.Threshold, t.WakeWord)
	} else {
		tpl = "нет (archi ear --train-wake)"
	}
	fmt.Printf("Бэкенд записи:      %s (device: %s, %d Гц)\n", backend, device, rate)
	fmt.Printf("Wake-движок:        %s | слова: %v\n", ear.WakeEngine, ear.WakeWords)
	fmt.Printf("DTW-шаблоны:        %s\n", tpl)
	fmt.Printf("STT:                enabled=%v base_url=%s model=%s\n",
		cfg.Voice.STT.Enabled, cfg.Voice.STT.BaseURL, cfg.Voice.STT.Model)
	fmt.Printf("TTS:                enabled=%v\n", cfg.Voice.TTS.Enabled)
	fmt.Printf("IPC-сокет:          %s\n", cfg.IPC.SocketPath)
	fmt.Printf("XDG_RUNTIME_DIR:    %q (пусто = PipeWire недоступен системному сервису)\n",
		os.Getenv("XDG_RUNTIME_DIR"))
	fmt.Println()

	// ---- 1. Захват 8 секунд с живым индикатором ----
	fmt.Println("─ Говорите что-нибудь в микрофон 8 секунд…")
	rec, err := startStreamRecorder(cfg.Voice.Mic, rate)
	if err != nil {
		fmt.Printf("❌ Рекордер не стартовал: %v\n%s\n", err, recorderHints())
		return err
	}
	defer rec.stop()

	frames := readFrames(ctx, rec, frameBytes)
	v := newVAD(ear.VAD)
	if v.learnMs > 1500 {
		v.learnMs = 1500 // калибровку ускоряем для теста
	}
	var peak, sum float64
	var n int
	var lastUtter []byte
	start := time.Now()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	captureOK := false
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-ticker.C:
			elapsed := time.Since(start).Seconds()
			if elapsed >= 8 {
				break loop
			}
			avg := 0.0
			if n > 0 {
				avg = sum / float64(n)
			}
			fmt.Printf("  %4.1fs %s  RMS=%.0f (peak=%.0f)\n", elapsed, levelBar(avg, peak), avg, peak)
		case frame, ok := <-frames:
			if !ok {
				break loop
			}
			captureOK = true
			r := rms(frame)
			if r > peak {
				peak = r
			}
			sum += r
			n++
			wasLearning := v.Learning()
			v.SetLastFrame(frame)
			if v.feed(r) == vadUtterance {
				lastUtter = v.takeUtterance()
			}
			if wasLearning && !v.Learning() {
				fmt.Printf("  ⚙ шумовой пол: RMS≈%.0f → речь ловится с RMS≈%.0f\n",
					v.floor, v.floor*ear.VAD.StartFactor)
			}
		}
	}

	fmt.Println()
	failed := false

	// ---- Вердикт 1: захват ----
	if !captureOK || n == 0 {
		fmt.Printf("❌ [1/5] Захват: поток пуст — микрофон не отдаёт кадры.\n%s\n", recorderHints())
		return fmt.Errorf("захват не работает")
	}
	fmt.Printf("✅ [1/5] Захват: %s отдал %d кадров (%.1f c)\n", rec.backend, n, float64(n)*float64(frameMs)/1000)

	// ---- Вердикт 2: слышит ли речь ----
	speechThr := v.floor * ear.VAD.StartFactor
	if v.floor <= 0 {
		v.floor = 1 // калибровка не успела — берём консервативный пол
	}
	if peak >= speechThr && peak > 50 {
		fmt.Printf("✅ [2/5] Микрофон слышит речь: peak RMS=%.0f ≥ порога %.0f\n", peak, speechThr)
	} else {
		failed = true
		fmt.Printf("❌ [2/5] Микрофон очень тихий: peak RMS=%.0f, порог речи≈%.0f\n"+
			"   → скажи громче в микрофон, проверь громкость входа (pactl set-source-volume)\n"+
			"   → или снизь ear.vad.start_factor (сейчас %.1f)\n", peak, speechThr, ear.VAD.StartFactor)
	}

	// ---- Вердикт 3: VAD собрал фразу ----
	if lastUtter != nil {
		ms := len(lastUtter) / 2 * 1000 / rate
		fmt.Printf("✅ [3/5] VAD: фраза собрана (%d мс)\n", ms)
	} else {
		fmt.Printf("⚠️ [3/5] VAD: фраза не собрана — говорил ли ты в эти 8 секунд?\n")
	}

	// ---- Вердикт 4: STT ----
	transcript := ""
	if lastUtter != nil && cfg.Voice.STT.Enabled {
		stt := NewTranscriber(cfg.Voice.STT, log)
		tr, err := stt.Transcribe(ctx, wrapWAV(lastUtter, rate), "ear-test.wav")
		if err != nil {
			failed = true
			fmt.Printf("❌ [4/5] STT: %v\n   → проверь base_url/ключ (env %s), "+
				"curl -F file=@test.wav %s/audio/transcriptions\n", err, "%ключ%", cfg.Voice.STT.BaseURL)
		} else {
			transcript = strings.TrimSpace(tr)
			fmt.Printf("✅ [4/5] STT: «%s»\n", transcript)
		}
	} else if !cfg.Voice.STT.Enabled {
		failed = true
		fmt.Printf("❌ [4/5] STT выключен (voice.stt.enabled=false) — команды не распознаются\n")
	} else {
		fmt.Printf("⚠️ [4/5] STT: нечего проверять (фразы не было)\n")
	}

	// ---- Вердикт 5: wake-word ----
	if transcript != "" {
		if rest, m := splitWakeWord(transcript, ear.WakeWords); m {
			fmt.Printf("✅ [5/5] Wake-word: услышан! команда после него: «%s»\n", rest)
		} else {
			fmt.Printf("❌ [5/5] Wake-word: фраза без «%s» — скажи «%s, …» в следующем прогоне\n",
				ear.WakeWords[0], ear.WakeWords[0])
		}
		// DTW-проверка на той же фразе.
		if t, err := LoadWakeTemplates(cfg.DataDir); err == nil {
			if d, m := t.MatchWake(lastUtter, rate); m {
				fmt.Printf("     DTW-движок: тоже узнал (расстояние %.2f ≤ порога %.2f)\n", d, t.Threshold)
			} else {
				fmt.Printf("     DTW-движок: не узнал (расстояние %.2f > порога %.2f)\n", d, t.Threshold)
			}
		}
	} else {
		fmt.Printf("⚠️ [5/5] Wake-word: не проверен (нет транскрипта)\n")
	}

	// ---- Вердикт 6: IPC ----
	if cfg.IPC.SocketPath != "" {
		c, err := net.DialTimeout("unix", cfg.IPC.SocketPath, 2*time.Second)
		if err != nil {
			failed = true
			fmt.Printf("❌ [6/5] IPC: сокет %s не отвечает: %v (systemctl status archi)\n",
				cfg.IPC.SocketPath, err)
		} else {
			c.Close()
			fmt.Printf("✅ [6/5] IPC: демон на связи (%s)\n", cfg.IPC.SocketPath)
		}
	}

	fmt.Println()
	if failed {
		return fmt.Errorf("диагностика нашла проблемы — см. ❌ выше")
	}
	fmt.Println("👂 Уши в порядке: агент должен слышать. Если в живом режиме молчит —")
	fmt.Println("   смотри journalctl -u archi-ear -f и сравни окружение с этим тестом")
	fmt.Println("   (запускай тест в тех же условиях, что и слушатель).")
	return nil
}

// RunEarTrain — обучение DTW-шаблонов wake-word голосом владельца.
func RunEarTrain(ctx context.Context, cfg config.Config, log *slog.Logger, samples int) error {
	ear := cfg.Ear
	if samples <= 0 {
		samples = 5
	}
	rate := cfg.Voice.Mic.SampleRate
	if rate == 0 {
		rate = 16000
	}
	frameMs := ear.VAD.FrameMs
	if frameMs <= 0 {
		frameMs = 30
	}
	frameBytes := rate / 1000 * frameMs * 2

	word := "арчи"
	if len(ear.WakeWords) > 0 {
		word = ear.WakeWords[0]
	}

	fmt.Println("═══ Обучение ушей: шаблоны wake-word ═══")
	fmt.Printf("Скажу «%s» %d раз, по одному на подсказку. Говори нормально, не кричи.\n", word, samples)
	fmt.Println("Между образцами — секунда тишины (VAD калибруется).")

	rec, err := startStreamRecorder(cfg.Voice.Mic, rate)
	if err != nil {
		fmt.Printf("❌ Рекордер не стартовал: %v\n%s\n", err, recorderHints())
		return err
	}
	defer rec.stop()
	frames := readFrames(ctx, rec, frameBytes)

	v := newVAD(ear.VAD)
	var collected [][][]float64

	for i := 0; i < samples; i++ {
		fmt.Printf("\nОбразец %d/%d — говори «%s»… ", i+1, samples, word)
		pcm, ok := waitUtterance(ctx, frames, v, frameMs, 8*time.Second)
		if !ok {
			fmt.Printf("не услышал (таймаут 8 с) — попробуй ещё, этот не считается\n")
			i--
			continue
		}
		ms := len(pcm) / 2 * 1000 / rate
		if ms < 250 || ms > 2000 {
			fmt.Printf("%d мс — слишком %s, повтори (нужно 0.3–2 с)\n", ms,
				map[bool]string{true: "длинно", false: "коротко"}[ms > 2000])
			i--
			continue
		}
		fr := mfcc(pcm, rate)
		if len(fr) == 0 {
			fmt.Printf("пустой сигнал\n")
			i--
			continue
		}
		collected = append(collected, fr)
		fmt.Printf("✓ %.1f с (%d кадров)\n", float64(ms)/1000, len(fr))
	}

	if len(collected) < 3 {
		return fmt.Errorf("собрано %d образцов — нужно минимум 3, повтори тренировку", len(collected))
	}

	// Кросс-валидация: расстояния между образцами одного слова.
	fmt.Println("\nКросс-валидация (расстояния между образцами):")
	maxD := 0.0
	for i := 0; i < len(collected); i++ {
		for j := i + 1; j < len(collected); j++ {
			d := dtwDistance(collected[i], collected[j])
			if math.IsInf(d, 1) {
				continue
			}
			fmt.Printf("  %d↔%d: %.2f\n", i+1, j+1, d)
			if d > maxD {
				maxD = d
			}
		}
	}
	threshold := maxD * 1.5
	if threshold < 3.5 {
		threshold = 3.5
	}
	if threshold > 8.5 {
		threshold = 8.5
	}

	t := &WakeTemplates{
		Version:   1,
		TrainedAt: time.Now(),
		WakeWord:  word,
		Threshold: math.Round(threshold*100) / 100,
		Samples:   collected,
	}
	if err := SaveWakeTemplates(cfg.DataDir, t); err != nil {
		return fmt.Errorf("не сохранить шаблоны: %w", err)
	}

	fmt.Printf("\n✅ Сохранено: %s (%d образцов)\n", cfg.DataDir+"/wake-templates.json", len(collected))
	fmt.Printf("Порог записан в файл: %.2f (межобразцовое %.2f + 50%% запаса)\n", t.Threshold, maxD)
	fmt.Printf("Если ловит плохо — подними порог в конфиге:\n  ear:\n    wake_threshold: %.2f\n", t.Threshold+1)
	fmt.Printf("Включи движок в конфиге:\n  ear:\n    wake_engine: dtw   # или both — dtw + STT\n")
	fmt.Println("И перезапусти слушатель: systemctl restart archi-ear")
	fmt.Println("Проверка: archi ear --test (скажи «" + word + "» — увидишь DTW-расстояние)")
	return nil
}

// waitUtterance ждёт одну фразу от VAD с таймаутом.
func waitUtterance(ctx context.Context, frames <-chan []byte, v *vad,
	frameMs int, timeout time.Duration) ([]byte, bool) {
	deadline := time.After(timeout)
	for {
		select {
		case <-ctx.Done():
			return nil, false
		case <-deadline:
			return nil, false
		case frame, ok := <-frames:
			if !ok {
				return nil, false
			}
			v.SetLastFrame(frame)
			if v.feed(rms(frame)) == vadUtterance {
				return v.takeUtterance(), true
			}
		}
	}
}
