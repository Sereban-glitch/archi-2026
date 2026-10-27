// ear.go — hands-free слушатель «Арчи, …» (фаза 2 голосового эталона).
//
// Отдельный процесс `archi ear` (deploy/archi-ear.service): непрерывно слушает
// микрофон, детектирует речь энергией (RMS-VAD с адаптивным шумовым полом),
// ловит wake-word и отправляет команду демону через IPC, отвечает голосом (TTS).
//
// Движки wake-word (ear.wake_engine):
//   - stt  — классика: фраза → STT → совпадение первого слова (нужен STT-провайдер);
//   - dtw  — офлайн: MFCC+DTW по шаблонам голоса владельца (archi ear --train-wake),
//     ноль зависимостей, работает без сети и ключей;
//   - http — сайдкар с моделью openWakeWord/livekit-wakeword (deploy/wake_server.py),
//     «Арчи» натренировывается на openwakeword.com за <1 часа;
//   - both — dtw ИЛИ stt (максимальная отзывчивость).
//
// Полудуплекс с barge-in: во время озвучки ответа кадры НЕ кормят основной VAD
// (эхо динамика не портит шумовой пол), но громкая устойчивая речь сверх
// отдельного порога прерывает плеер. После ответа действует cooldown — анти-эхо.
package voice

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
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
	binary.LittleEndian.PutUint16(hdr[20:], 1)            // PCM
	binary.LittleEndian.PutUint16(hdr[22:], 1)            // mono
	binary.LittleEndian.PutUint32(hdr[24:], uint32(rate)) // rate
	binary.LittleEndian.PutUint32(hdr[28:], uint32(rate*2))
	binary.LittleEndian.PutUint16(hdr[32:], 2)  // block align
	binary.LittleEndian.PutUint16(hdr[34:], 16) // bits
	copy(hdr[36:], "data")
	binary.LittleEndian.PutUint32(hdr[40:], uint32(len(pcm)))
	return append(hdr, pcm...)
}

// latinWake — доменная транслитерация латиницы в кириллицу для wake-word:
// «archi» → «арчи», «archy» → «арчи». c→ч и h-пропуск специфичны для «Арчи»,
// для остальных букв — фонетическая таблица.
var latinWake = strings.NewReplacer(
	"a", "а", "b", "б", "c", "ч", "d", "д", "e", "е", "f", "ф",
	"g", "г", "h", "", "i", "и", "j", "й", "k", "к", "l", "л",
	"m", "м", "n", "н", "o", "о", "p", "р", "q", "к", "r", "р",
	"s", "с", "t", "т", "u", "у", "v", "в", "w", "в", "x", "кс",
	"y", "и", "z", "з",
)

// splitWakeWord проверяет, начинается ли фраза с wake-word.
// Терпим к раскладке (archi), опечаткам (арче — Левенштейн ≤1) и
// склейкам/разрывам пробелами («ар чи»). Возвращает (команду, true) при матче.
// ВНИМАНИЕ: транслитерация применяется ТОЛЬЬКО к кандидату-слову —
// остальной текст команды не искажается («status» остаётся «status»).)
func splitWakeWord(transcript string, words []string) (string, bool) {
	norm := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		s = strings.NewReplacer("ё", "е", "-", " ", ",", " ", ".", " ",
			"!", " ", "?", " ", ":", " ", ";", " ").Replace(s)
		return strings.Join(strings.Fields(s), " ")
	}
	tok := strings.SplitN(norm(transcript), " ", 3)
	if len(tok) == 0 || tok[0] == "" {
		return "", false
	}
	first, joined := tok[0], tok[0]
	if len(tok) >= 2 {
		joined += tok[1]
	}
	// Транслит — только над кандидатами, не над командой.
	firstT := latinWake.Replace(first)
	joinedT := latinWake.Replace(joined)
	restAfter := func(n int) string {
		if len(tok) > n {
			return strings.TrimSpace(strings.Join(tok[n:], " "))
		}
		return ""
	}
	for _, w := range words {
		wn := norm(w)
		if wn == "" {
			continue
		}
		switch {
		case first == wn || firstT == wn || first == latinWake.Replace(wn):
			return restAfter(1), true
		case joined == wn || joinedT == wn:
			return restAfter(2), true
		case len(first) >= len(wn)-1 &&
			(editDistance(first, wn) <= 1 || editDistance(firstT, wn) <= 1):
			return restAfter(1), true
		case len(joinedT) >= len(wn) && editDistance(joinedT, wn) <= 1:
			return restAfter(2), true
		}
	}
	return "", false
}

// stripFirstToken срезает первое слово фразы (для движков dtw/http:
// wake-word пойман акустически, но в транскрипте не распознан —
// срезаем «арче»/«арчи и» и отправляем остаток как команду).
func stripFirstToken(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, ' '); i > 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return ""
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

// streamRecorder — живой процесс-источник PCM (своя процессная группа).
type streamRecorder struct {
	cmd     *exec.Cmd
	stdout  io.ReadCloser
	stderr  *bytes.Buffer
	backend string
	device  string
}

// stop гасит весь граф рекордера и дожидается завершения.
func (r *streamRecorder) stop() {
	if r.cmd != nil && r.cmd.Process != nil {
		_ = syscall.Kill(-r.cmd.Process.Pid, syscall.SIGTERM)
		done := make(chan struct{})
		go func() {
			_, _ = r.cmd.Process.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = syscall.Kill(-r.cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
	}
}

// clipE укорачивает строку для лога.
func clipE(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// errText — человеческое объяснение смерти рекордера (с stderr и подсказками).
func (r *streamRecorder) errText(err error) error {
	msg := fmt.Errorf("поток %s (device=%s) оборвался: %w", r.backend, r.device, err)
	if se := strings.TrimSpace(r.stderr.String()); se != "" {
		msg = fmt.Errorf("%w | stderr: %s", msg, clipE(se, 300))
	}
	return msg
}

// startStreamRecorder поднимает рекордер, пишущий s16le/mono/rate на stdout.
func startStreamRecorder(cfg config.VoiceMicConfig, rate int) (*streamRecorder, error) {
	backend, err := resolveBackend(cfg.Backend)
	if err != nil {
		return nil, err
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
		args := []string{"--raw",
			"--rate", fmt.Sprintf("%d", rate), "--channels", "1",
			"--format=s16le"}
		// ФИКС: раньше --device терялся и parecord брал дефолтный источник.
		if device != "" && device != "default" {
			args = append(args, "--device="+device)
		}
		args = append(args, "-")
		cmd = exec.Command("parecord", args...)
	case "ffmpeg":
		cmd = exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
			"-f", "alsa", "-i", device,
			"-ar", fmt.Sprintf("%d", rate), "-ac", "1",
			"-f", "s16le", "pipe:1")
	default:
		return nil, fmt.Errorf("ear: бэкенд %q не поддерживает стриминг", backend)
	}
	// Своя процессная группа — гасим весь граф рекордера одним сигналом.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("ear: stdout рекордера: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ear: старт %s: %w (stderr: %s)",
			backend, err, clipE(stderr.String(), 200))
	}
	return &streamRecorder{cmd: cmd, stdout: stdout, stderr: &stderr, backend: backend, device: device}, nil
}

// recorderHints — что проверить, если микрофонный поток умирает.
func recorderHints() string {
	return "проверь: 1) arecord -L (выбери plughw:... → ear.device) 2) для systemd-сервиса нужен " +
		"Environment=XDG_RUNTIME_DIR=/run/user/1000 (PipeWire) либо юнит пользователя " +
		"deploy/archi-ear-user.service 3) пользователь в группе audio 4) archi ear --test"
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
	engine := ear.WakeEngine

	// Стартовая диагностика: сразу видно, чем слушаем и что может мешать.
	if os.Getenv("XDG_RUNTIME_DIR") == "" && os.Getenv("PULSE_SERVER") == "" {
		log.Warn("ear: XDG_RUNTIME_DIR пуст — дефолтный микрофон PipeWire недоступен " +
			"из системного сервиса. Лекарства: юнит пользователя (deploy/archi-ear-user.service), " +
			"или Environment=XDG_RUNTIME_DIR=/run/user/1000, или ear.device=plughw:... (arecord -L)")
	}

	// DTW-шаблоны (движки dtw/both).
	var templates *WakeTemplates
	if engine == "dtw" || engine == "both" {
		t, err := LoadWakeTemplates(cfg.DataDir)
		if err != nil {
			if engine == "dtw" {
				return fmt.Errorf("%w — сначала обучи уши: archi ear --train-wake", err)
			}
			log.Error("ear: dtw-шаблоны не загрузились, слушаю только по STT", "err", err)
		} else {
			if ear.WakeThreshold > 0 {
				t.Threshold = ear.WakeThreshold // ручная подстройка из конфига
			}
			templates = t
			log.Info("ear: dtw-шаблоны загружены", "samples", len(t.Samples),
				"threshold", t.Threshold, "word", t.WakeWord)
		}
	}
	if ear.WakeEngine == "http" {
		log.Info("ear: wake-сайдкар", "url", ear.WakeServerURL,
			"hint", "модель «Арчи»: openwakeword.com или livekit-wakeword, deploy/wake_server.py")
	}

	// STT нужен для текста команды (кроме чистого wake-режима); пробуем один раз
	// при старте, чтобы глухой провайдер был виден в журнале сразу, а не «по факту».
	var stt *Transcriber
	if cfg.Voice.STT.Enabled {
		stt = NewTranscriber(cfg.Voice.STT, log)
	} else {
		log.Error("ear: voice.stt выключен — команды голосом НЕ распознаются " +
			"(wake-word можно ловить движком dtw/http)")
	}

	// Сессии рекордера: если поток умирает (USB-микрофон, смена источника
	// PipeWire, suspend) — поднимаем заново, не отдавая процесс на рестарт
	// systemd (Restart=always имеет StartLimitBurst и может сдаться навсегда).
	for {
		if ctx.Err() != nil {
			return nil
		}
		err := runEarSession(ctx, cfg, engine, templates, stt, log)
		if ctx.Err() != nil {
			return nil
		}
		log.Error("ear: сессия рекордера прервана, перезапуск через 3 с", "err", err,
			"hints", recorderHints())
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(3 * time.Second):
		}
	}
}

// runEarSession — один цикл жизни рекордера: слушать → ловить wake → отвечать.
func runEarSession(ctx context.Context, cfg config.Config, engine string,
	templates *WakeTemplates, stt *Transcriber, log *slog.Logger) error {

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

	rec, err := startStreamRecorder(cfg.Voice.Mic, rate)
	if err != nil {
		return err
	}
	defer rec.stop()
	log.Info("ear: слушаю микрофон", "backend", rec.backend, "device", rec.device,
		"engine", engine, "wake_words", ear.WakeWords,
		"rate", rate, "frame_ms", frameMs, "vad", "rms")

	var tts *Synthesizer
	if cfg.Voice.TTS.Enabled {
		tts = NewSynthesizer(cfg.Voice.TTS, log)
	}

	v := newVAD(ear.VAD)
	v.setMaxUtterance(int(ear.MaxUtterance.Duration.Milliseconds()))
	bargeIn := ear.BargeIn == nil || *ear.BargeIn

	frames := make(chan []byte, 128)
	streamErr := make(chan error, 1)
	// Читатель потока: если слушатель занят (STT/LLM), канал переполняется
	// и кадры теряются — рекордер не блокируется, это осознанно.
	go func() {
		defer close(frames)
		buf := make([]byte, frameBytes)
		for {
			if _, err := io.ReadFull(rec.stdout, buf); err != nil {
				if ctx.Err() == nil {
					select {
					case streamErr <- rec.errText(err):
					default:
					}
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
				select {
				case err := <-streamErr:
					return err
				default:
					return fmt.Errorf("ear: рекордер остановился без объяснений")
				}
			}

			// Полудуплекс: во время озвучки VAD не кормим (эхо!), но ловим
			// устойчивую громкую речь — это перебивание владельца.
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

			wasLearning := v.Learning()
			v.SetLastFrame(frame)
			event := v.feed(rms(frame))
			if wasLearning && !v.Learning() {
				log.Info("ear: шумовой пол откалиброван",
					"floor_rms", math.Round(v.floor),
					"речь_начинается_с_rms", math.Round(v.floor*ear.VAD.StartFactor),
					"note", "если говоришь громче, а фразы не появляются — подними микрофон/громкость")
			}
			if event != vadUtterance {
				continue
			}
			pcm := v.takeUtterance()

			// Транскрипт нужен почти всегда: движок stt — для wake, остальные —
			// для текста команды. Сбой STT не роняет локальные движки.
			transcript := ""
			if stt != nil {
				tr, err := stt.Transcribe(ctx, wrapWAV(pcm, rate), "ear.wav")
				if err != nil {
					log.Warn("ear: распознавание не удалось (команда может потеряться)", "err", err)
				} else {
					transcript = strings.TrimSpace(tr)
				}
			}
			if transcript != "" {
				log.Debug("ear: услышал фразу", "transcript", clipE(transcript, 160))
			}

			// ---- детекция wake-word движком ----
			// Семантика строгая: stt — совпадение слова в транскрипте; dtw —
			// акустика (транскрипт идёт только в текст команды); http — модель
			// в сайдкаре; both — любой из stt/dtw. Местный wake не зависит от облака.
			wake := false
			command := ""
			if engine == "stt" || engine == "both" {
				if rest, m := splitWakeWord(transcript, ear.WakeWords); m {
					wake, command = true, rest
				}
			}
			if !wake && templates != nil && (engine == "dtw" || engine == "both") {
				if d, m := templates.MatchWake(pcm, rate); m {
					wake = true
					log.Info("ear: wake-word по шаблону (dtw)", "dist", math.Round(d*100)/100,
						"threshold", templates.Threshold)
					if command == "" && transcript != "" {
						command = stripFirstToken(transcript)
					}
				}
			}
			if !wake && engine == "http" {
				r, err := httpWakeDetect(ctx, ear.WakeServerURL, wrapWAV(pcm, rate))
				if err != nil {
					log.Warn("ear: wake-сайдкар не ответил, пробую по транскрипту", "err", err)
					if rest, m := splitWakeWord(transcript, ear.WakeWords); m {
						wake, command = true, rest
					}
				} else if r.Detected {
					wake = true
					log.Info("ear: wake-word по модели (http)", "keyword", r.Keyword,
						"score", math.Round(r.Score*100)/100)
					if command == "" && transcript != "" {
						command = stripFirstToken(transcript)
					}
				}
			}

			switch {
			case wake && command != "":
				log.Info("ear: команда", "text", command)
			case wake:
				log.Info("ear: wake-word услышан, жду команду")
				armed = true
				continue
			case armed:
				if transcript == "" {
					log.Error("ear: фраза после активации не распознана — STT недоступен?",
						"hints", "проверь voice.stt (base_url/ключ), archi ear --test")
					speak("Не смог распознать команду")
					continue
				}
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
