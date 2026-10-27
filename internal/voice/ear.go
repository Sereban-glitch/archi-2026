// ear.go — hands-free слушатель «Арчи, …» (фаза 2 голосового эталона).
//
// Отдельный процесс `archi ear` (deploy/archi-ear.service): непрерывно слушает
// микрофон, детектирует речь энергией (RMS-VAD с адаптивным шумовым полом),
// распознаёт короткие фразы через STT-цепочку и, услышав wake-word («арчи»),
// отправляет команду демону через IPC и отвечает голосом (TTS).
//
// Почему VAD на энергии, а не Silero: чистый Go, ноль моделей и зависимостей,
// на i5-6200U почти не ест CPU; Silero/openWakeWord — следующий шаг, интерфейс
// VAD-стейт-машины (feed кадра → события) это позволяет.
//
// Полудуплекс с barge-in: во время озвучки ответа кадры НЕ кормят основной VAD
// (эхо динамика не портит шумовой пол и не собирается в «фразы»), но громкая
// устойчивая речь сверх отдельного порога прерывает плеер — пользователь
// может перебить агента. После ответа действует cooldown — анти-эхо пауза.
package voice

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/archi/archi/internal/config"
	"github.com/archi/archi/internal/ipc"
)

// ---- VAD: стейт-машина на кадрах RMS ----

type vadEvent int

const (
	vadNone        vadEvent = iota // ничего интересного
	vadSpeechStart                 // началась речь
	vadUtterance                   // закончилась целая фраза (готов PCM)
)

// vad — энергийный детектор речи с адаптивным шумовым полом.
// Фазы: learn (калибровка тишины) → idle → inSpeech.
// НЕ потокобезопасен: один VAD — один цикл чтения кадров.
type vad struct {
	frameMs   int
	startFact float64
	stopFact  float64
	minSpeech int // мс
	silenceMs int // мс тишины для конца фразы
	learnMs   int // калибровка
	maxMs     int // жёсткий лимит фразы

	floor    float64 // шумовой пол (RMS)
	learnSum float64
	learnCnt int
	learning bool
	inSpeech bool
	silence  int // мс тишины внутри фразы
	speechMs int // мс собственно речи в фразе
	utterMs  int // мс с начала фразы

	buf       []byte // накопленная фраза (PCM)
	pre       []byte // pre-roll ~400 мс до триггера — не резать начало wake-word
	preCap    int    // кадров pre-roll
	lastFrame []byte // текущий кадр (ставится перед feed)
	utter     []byte // завершённая фраза — забирается takeUtterance
}

func newVAD(cfg config.EarVADConfig) *vad {
	v := &vad{
		frameMs:   cfg.FrameMs,
		startFact: cfg.StartFactor,
		stopFact:  cfg.StopFactor,
		minSpeech: cfg.MinSpeechMs,
		silenceMs: cfg.SilenceMs,
		learnMs:   cfg.NoiseLearnS * 1000,
	}
	if v.frameMs <= 0 {
		v.frameMs = 30
	}
	if v.startFact <= 0 {
		v.startFact = 3.2
	}
	if v.stopFact <= 0 {
		v.stopFact = 1.9
	}
	if v.minSpeech <= 0 {
		v.minSpeech = 320
	}
	if v.silenceMs <= 0 {
		v.silenceMs = 1100
	}
	if v.learnMs <= 0 {
		v.learnMs = 2000
	}
	v.preCap = 400 / v.frameMs
	v.learning = v.learnMs > 0
	return v
}

// setMaxUtterance подключает лимит длины фразы (мс).
func (v *vad) setMaxUtterance(ms int) { v.maxMs = ms }

// Floor возвращает текущий шумовой пол (для barge-порога).
func (v *vad) Floor() float64 { return v.floor }

// Learning reports whether the calibration phase is still running.
func (v *vad) Learning() bool { return v.learning }

// SetLastFrame ставит кадр PCM, который feed приписывает в буфер.
func (v *vad) SetLastFrame(pcm []byte) { v.lastFrame = pcm }

// resetSpeech сбрасывает текущую фразу (после barge-in).
func (v *vad) resetSpeech() {
	v.inSpeech = false
	v.buf = v.buf[:0]
	v.pre = v.pre[:0]
	v.silence, v.speechMs, v.utterMs = 0, 0, 0
}

// feed обрабатывает один кадр RMS и выдаёт событие.
func (v *vad) feed(rms float64) vadEvent {
	// Фаза 1: калибровка шума.
	if v.learning {
		v.learnSum += rms
		v.learnCnt++
		if v.learnCnt*v.frameMs >= v.learnMs {
			v.floor = math.Max(v.learnSum/float64(v.learnCnt), 1e-6)
			v.learning = false
		}
		return vadNone
	}

	frame := len(v.lastFrame)
	if frame == 0 {
		return vadNone
	}

	if !v.inSpeech {
		// Медленная подстройка пола в тишине — не на речи.
		if rms < v.floor*v.stopFact {
			v.floor += (rms - v.floor) * 0.003
		}
		if rms > v.floor*v.startFact {
			v.inSpeech = true
			v.silence, v.speechMs, v.utterMs = 0, 0, 0
			v.buf = append(append([]byte(nil), v.pre...), v.lastFrame...)
			v.pre = v.pre[:0]
			return vadSpeechStart
		}
		// держим pre-roll: последние ~400 мс тишины/шума перед триггером
		v.pre = append(v.pre, v.lastFrame...)
		if over := len(v.pre) - v.preCap*frame; over > 0 {
			v.pre = v.pre[over:]
		}
		return vadNone
	}

	// Внутри фразы.
	v.buf = append(v.buf, v.lastFrame...)
	v.utterMs += v.frameMs
	if rms >= v.floor*v.stopFact {
		v.silence = 0
		v.speechMs += v.frameMs
	} else {
		v.silence += v.frameMs
	}
	if v.silence >= v.silenceMs || (v.maxMs > 0 && v.utterMs >= v.maxMs) {
		done := v.speechMs >= v.minSpeech
		out := v.buf
		v.inSpeech = false
		v.buf = nil
		if done {
			v.utter = out
			return vadUtterance
		}
		// слишком коротко — шум/клик, выкидываем
	}
	return vadNone
}

// takeUtterance забирает накопленную фразу.
func (v *vad) takeUtterance() []byte {
	out := v.utter
	v.utter = nil
	if out == nil {
		out = []byte{}
	}
	return out
}

// rms — среднеквадратичное значение кадров s16le.
func rms(pcm []byte) float64 {
	if len(pcm) < 2 {
		return 0
	}
	var sum float64
	n := 0
	for i := 0; i+1 < len(pcm); i += 2 {
		s := float64(int16(binary.LittleEndian.Uint16(pcm[i:])))
		sum += s * s
		n++
	}
	if n == 0 {
		return 0
	}
	return math.Sqrt(sum / float64(n))
}

// wrapWAV оборачивает сырой PCM s16le в WAV-заголовок (для STT).
func wrapWAV(pcm []byte, rate int) []byte {
	if rate == 0 {
		rate = 16000
	}
	hdr := make([]byte, 44)
	copy(hdr[0:], "RIFF")
	binary.LittleEndian.PutUint32(hdr[4:], uint32(36+len(pcm)))
	copy(hdr[8:], "WAVE")
	copy(hdr[12:], "fmt ")
	binary.LittleEndian.PutUint32(hdr[16:], 16)
	binary.LittleEndian.PutUint16(hdr[20:], 1)              // PCM
	binary.LittleEndian.PutUint16(hdr[22:], 1)              // mono
	binary.LittleEndian.PutUint32(hdr[24:], uint32(rate))   // rate
	binary.LittleEndian.PutUint32(hdr[28:], uint32(rate*2)) // bytes/sec
	binary.LittleEndian.PutUint16(hdr[32:], 2)              // block align
	binary.LittleEndian.PutUint16(hdr[34:], 16)             // bits
	copy(hdr[36:], "data")
	binary.LittleEndian.PutUint32(hdr[40:], uint32(len(pcm)))
	return append(hdr, pcm...)
}

// splitWakeWord проверяет, начинается ли фраза с wake-word.
// Возвращает (остаток-команду, true) при матче; («», false) — мимо.
func splitWakeWord(transcript string, words []string) (string, bool) {
	norm := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		s = strings.NewReplacer("ё", "е", "-", " ", ",", " ", ".", " ",
			"!", " ", "?", " ", ":", " ").Replace(s)
		return strings.Join(strings.Fields(s), " ")
	}
	tok := strings.SplitN(norm(transcript), " ", 2)
	if len(tok) == 0 || tok[0] == "" {
		return "", false
	}
	first := tok[0]
	rest := ""
	if len(tok) == 2 {
		rest = strings.TrimSpace(tok[1])
	}
	for _, w := range words {
		wn := norm(w)
		if wn == "" {
			continue
		}
		if first == wn || (len(first) >= 4 && editDistance(first, wn) <= 1) {
			return rest, true
		}
	}
	return "", false
}

// editDistance — Левенштейн (короткие слова, O(n*m) не жалко).
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

func min3(a, b, c int) int {
	if a < b && a < c {
		return a
	}
	if b < c {
		return b
	}
	return c
}

// ---- стриминговый рекордер (raw PCM на stdout) ----

// startStreamRecorder поднимает рекордер, пишущий s16le/mono/rate на stdout.
func startStreamRecorder(cfg config.VoiceMicConfig, rate int) (*exec.Cmd, io.ReadCloser, error) {
	backend, err := resolveBackend(cfg.Backend)
	if err != nil {
		return nil, nil, err
	}
	device := cfg.Device
	if device == "" {
		device = "default"
	}
	var cmd *exec.Cmd
	switch backend {
	case "arecord":
		cmd = exec.Command("arecord", "-q", "-f", "S16_LE",
			"-r", fmt.Sprintf("%d", rate), "-c", "1", "-D", device, "-t", "raw", "-")
	case "parecord":
		cmd = exec.Command("parecord", "--raw",
			"--rate", fmt.Sprintf("%d", rate), "--channels", "1",
			"--format=s16le", "-")
	case "ffmpeg":
		cmd = exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
			"-f", "alsa", "-i", device,
			"-ar", fmt.Sprintf("%d", rate), "-ac", "1",
			"-f", "s16le", "pipe:1")
	default:
		return nil, nil, fmt.Errorf("ear: бэкенд %q не поддерживает стриминг", backend)
	}
	// Своя процессная группа — гасим весь граф рекордера одним сигналом.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("ear: stdout рекордера: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("ear: старт %s: %w (stderr: %s)",
			backend, err, clipE(stderr.String(), 200))
	}
	return cmd, stdout, nil
}

func clipE(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// askAgent отправляет команду демону через IPC-сокет (с отменой по ctx).
func askAgent(ctx context.Context, socket, text string) (string, error) {
	type res struct {
		reply string
		err   error
	}
	ch := make(chan res, 1)
	go func() {
		reply, err := ipc.RunCLIQuiet(socket, text, false, false)
		ch <- res{reply, err}
	}()
	select {
	case r := <-ch:
		return r.reply, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// ---- главный цикл ----

// RunEar — hands-free слушатель. Блокирует до отмены ctx.
func RunEar(ctx context.Context, cfg config.Config, log *slog.Logger) error {
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

	cmd, stdout, err := startStreamRecorder(cfg.Voice.Mic, rate)
	if err != nil {
		return err
	}
	defer func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		}
		_, _ = cmd.Process.Wait()
	}()
	log.Info("ear: слушаю микрофон", "wake_words", ear.WakeWords,
		"rate", rate, "frame_ms", frameMs, "vad", "rms")

	stt := NewTranscriber(cfg.Voice.STT, log)
	var tts *Synthesizer
	if cfg.Voice.TTS.Enabled {
		tts = NewSynthesizer(cfg.Voice.TTS, log)
	}

	v := newVAD(ear.VAD)
	v.setMaxUtterance(int(ear.MaxUtterance.Duration.Milliseconds()))
	bargeIn := ear.BargeIn == nil || *ear.BargeIn

	frames := make(chan []byte, 128)
	// Читатель потока: если слушатель занят (STT/LLM), канал переполняется
	// и кадры теряются — рекордер не блокируется, это осознанно.
	go func() {
		defer close(frames)
		buf := make([]byte, frameBytes)
		for {
			if _, err := io.ReadFull(stdout, buf); err != nil {
				if ctx.Err() == nil {
					log.Error("ear: поток микрофона оборвался", "err", err)
				}
				return
			}
			frame := make([]byte, len(buf))
			copy(frame, buf)
			select {
			case frames <- frame:
			default:
			}
		}
	}()

	// Состояние воспроизведения/глушения (общее с goroutine озвучки).
	var playMu sync.Mutex
	var playCancel context.CancelFunc
	var muteUntilNs atomic.Int64
	mute := func(d time.Duration) { muteUntilNs.Store(time.Now().Add(d).UnixNano()) }
	muted := func() bool { return time.Now().UnixNano() < muteUntilNs.Load() }

	speak := func(text string) {
		if tts == nil {
			return
		}
		text = strings.TrimSpace(PlainText(text))
		if text == "" {
			return
		}
		go func() {
			audio, err := tts.Synthesize(ctx, text)
			if err != nil {
				log.Warn("ear: TTS не справился (отвечаю молча)", "err", err)
				return
			}
			playMu.Lock()
			if ctx.Err() != nil {
				playMu.Unlock()
				return
			}
			playCtx, cancel := context.WithCancel(ctx)
			playCancel = cancel
			playMu.Unlock()
			if err := Play(playCtx, audio, playExt(cfg.Voice.TTS.Format), log); err != nil {
				log.Warn("ear: не проиграть ответ", "err", err)
			}
			playMu.Lock()
			playCancel = nil
			playMu.Unlock()
			mute(ear.Cooldown.Duration) // анти-эхо после собственного ответа
		}()
	}

	armed := false // голый wake-word: следующая фраза = команда
	loudRun := 0   // подряд громкие кадры при воспроизведении (barge-in)

	for {
		select {
		case <-ctx.Done():
			return nil
		case frame, ok := <-frames:
			if !ok {
				return fmt.Errorf("ear: рекордер остановился")
			}

			// Полудуплекс: во время озвучки VAD не кормим (эхо!), но ловим
			// устойчивую громкую речь — это перебивание владельца.
			// Порог выше обычного старта (×1.8) и дольше (8 кадров ≈ 240 мс),
			// чтобы собственный динамик не перебивал сам себя.
			playMu.Lock()
			playing := playCancel != nil
			playMu.Unlock()
			if playing {
				if bargeIn {
					if rms(frame) > v.Floor()*ear.VAD.StartFactor*1.8 {
						loudRun++
					} else {
						loudRun = 0
					}
					if loudRun >= 8 {
						log.Info("ear: barge-in — прерываю озвучку")
						playMu.Lock()
						if playCancel != nil {
							playCancel()
						}
						playMu.Unlock()
						v.resetSpeech()
						loudRun = 0
						mute(300 * time.Millisecond) // хвост динамика затухает
					}
				}
				continue
			}
			if muted() {
				continue // эхо/cooldown — кадры мимо VAD
			}

			v.SetLastFrame(frame)
			if v.feed(rms(frame)) != vadUtterance {
				continue
			}
			pcm := v.takeUtterance()

			transcript, err := stt.Transcribe(ctx, wrapWAV(pcm, rate), "ear.wav")
			if err != nil {
				log.Warn("ear: распознавание не удалось", "err", err)
				continue
			}
			transcript = strings.TrimSpace(transcript)
			if transcript == "" {
				continue
			}
			command, isWake := splitWakeWord(transcript, ear.WakeWords)
			switch {
			case isWake && command != "":
				log.Info("ear: команда", "text", command)
			case isWake:
				log.Info("ear: wake-word услышан, жду команду")
				armed = true
				continue
			case armed:
				command = transcript
				log.Info("ear: команда (после активации)", "text", command)
			default:
				continue // фоновый разговор — не наше
			}
			armed = false

			reply, err := askAgent(ctx, cfg.IPC.SocketPath, command)
			if err != nil {
				log.Error("ear: демон не ответил", "err", err)
				speak("Демон не отвечает. Проверь статус.")
				continue
			}
			log.Info("ear: ответ", "reply", clipE(reply, 200))
			speak(reply)
		}
	}
}

// playExt — расширение temp-файла плеера по формату TTS.
func playExt(format string) string {
	switch strings.ToLower(format) {
	case "mp3":
		return ".mp3"
	case "wav":
		return ".wav"
	case "flac":
		return ".flac"
	case "aac":
		return ".aac"
	case "pcm":
		return ".raw"
	default:
		return ".oga" // opus
	}
}
