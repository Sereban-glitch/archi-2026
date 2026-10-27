// ptt.go — push-to-talk по хоткею (релиз 2026.16 «Push-To-Talk»).
//
// Зачем: hands-free wake-word «Арчи» через комнату искажается акустикой
// (Whisper слышит «а царь…», «унохбук…»), а кричать неудобно. Кнопка —
// надёжнее голоса: нажал Super+A → бип → сказал → замолчал → ответ вслух.
//
// archi ptt (синоним: archi voice --listen):
//  1. бип «слушаю» (мягкая восходящая терция, ~180 мс);
//  2. запись с микрофона (стриминговый рекордер ear);
//  3. умный VAD: автостоп по хвосту тишины (ptt.silence_ms, дефолт 800 мс)
//     ИЛИ по повторному нажатию хоткея (тоггл: второй запуск видит живую
//     сессию в lock-файле и шлёт ей SIGUSR1 — финализировать фразу);
//  4. STT → демону через IPC с флагом voice («РЕЖИМ УШЕЙ»: короткий
//     разговорный ответ без markdown);
//  5. озвучка ответа (стриминг Piper, как в ear).
//
// Повторное нажатие во время «думания» гасит ответ («замолчи»).
// Привычка сильнее кнопки: ведущее «Арчи, …» срезается из транскрипта.
package voice

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/archi/archi/internal/config"
)

// ErrPTTSignaled — нажатие пришлось на живую сессию: она получила SIGUSR1
// и останавливается (финализирует запись или гасит озвучку). Не ошибка.
var ErrPTTSignaled = errors.New("живая ptt-сессия остановлена повторным нажатием")

// ---- звуковые сигналы: короткие, мягкие, без щелчков ----

const beepRate = 16000

// synthBeep синтезирует WAV с последовательностью тонов. Атака/спад ~1/12
// тона — рампы против щелчков; амплитуда занижена (0.2 = «мягкий бип»,
// а не системный вопль).
func synthBeep(amp float64, toneMs int, freqs ...float64) []byte {
	per := beepRate * toneMs / 1000
	if per < 8 || len(freqs) == 0 {
		return nil
	}
	fade := per / 12
	if fade < 1 {
		fade = 1
	}
	pcm := make([]byte, per*len(freqs)*2)
	for i := 0; i < per*len(freqs); i++ {
		f := freqs[i/per]
		pos := i % per
		env := 1.0
		switch {
		case pos < fade:
			env = float64(pos) / float64(fade)
		case pos > per-fade:
			env = float64(per-pos) / float64(fade)
		}
		v := amp * env * math.Sin(2*math.Pi*f*float64(pos)/float64(beepRate))
		s := int16(v * 32767)
		binary.LittleEndian.PutUint16(pcm[2*i:], uint16(s))
	}
	return wrapWAV(pcm, beepRate)
}

var (
	beepStart = synthBeep(0.20, 90, 880, 1174.66)    // «слушаю» — восходящая терция
	beepStop  = synthBeep(0.20, 90, 1174.66, 880)    // «принял» — нисходящая
	beepError = synthBeep(0.22, 120, 329.63, 246.94) // «не вышло» — низ
)

// playBeep — бип не должен ломать сессию: любые ошибки — только в журнал.
func playBeep(ctx context.Context, wav []byte, log *slog.Logger) {
	if len(wav) < 44 {
		return
	}
	bctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := Play(bctx, wav, ".wav", log); err != nil {
		log.Warn("ptt: бип не проигрался (не критично)", "err", err)
	}
}

// ---- PTT VAD: пишем всё с нажатия, стопим по хвосту тишины ----

// pttVAD — детектор «сказал и замолчал» для записи по кнопке. Отличие от
// ear-VAD: не ловим начало речи (оно не нужно — буферизуем всё с нажатия),
// а ждём окончания: речь накапливается, затем тишина ≥ silenceMs → стоп.
// Чувствительность выше ear (start_factor 2.0 против 3.2): кнопка = намерение,
// ложный «speech» лишь чуть оттянет автостоп — цена ошибки мала.
type pttVAD struct {
	frameMs     int
	silenceMs   int
	startFact   float64
	minSpeechMs int

	floor      float64 // адаптивный шумовой пол (EMA по тихим кадрам)
	speechMs   int     // накоплено речевых мс (до minSpeech — защита от клика)
	speechSeen bool
	silenceRun int // мс тишины после признанной речи
}

func newPTTVAD(cfg config.PTTConfig) *pttVAD {
	v := &pttVAD{
		frameMs:     30,
		silenceMs:   cfg.SilenceMs,
		startFact:   cfg.StartFactor,
		minSpeechMs: 240,
		floor:       60, // стартовый пол: адаптируется к комнате за ~1 с тишины
	}
	if v.silenceMs <= 0 {
		v.silenceMs = 800
	}
	if v.startFact <= 0 {
		v.startFact = 2.0
	}
	if v.frameMs <= 0 {
		v.frameMs = 30
	}
	return v
}

// feed обрабатывает кадр; true — «после речи наступила достаточно долгая
// тишина, запись можно финализировать».
func (p *pttVAD) feed(rmsv float64) bool {
	if rmsv > p.floor*p.startFact {
		p.speechMs += p.frameMs
		p.silenceRun = 0
		if !p.speechSeen && p.speechMs >= p.minSpeechMs {
			p.speechSeen = true
		}
		return false
	}
	// Тихий кадр: пол медленно подтягивается к комнате (только на тишине).
	p.floor += (rmsv - p.floor) * 0.02
	if p.floor < 20 {
		p.floor = 20
	}
	if p.speechSeen {
		p.silenceRun += p.frameMs
		return p.silenceRun >= p.silenceMs
	}
	return false
}

// SpeechStarted — была ли признанная речь (управляет no_speech_timeout).
func (p *pttVAD) SpeechStarted() bool { return p.speechSeen }

// ---- тоггл: повторное нажатие хоткея ----

// pttLockPath — lock живой сессии. XDG_RUNTIME_DIR (tmpfs, живёт в сессии
// Cinnamon, из которой и дёргается хоткей) с фолбэком в /tmp.
func pttLockPath() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = os.TempDir()
	}
	return filepath.Join(dir, fmt.Sprintf("archi-ptt-%d.lock", os.Getuid()))
}

// pttProcAlive — жив ли процесс pid и наш ли он вообще (защита от re-use pid:
// ядро могло отдать pid чужому процессу, SIGUSR1 которому — плохая идея).
func pttProcAlive(pid int) bool {
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		return false
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	cmd := strings.ToLower(strings.ReplaceAll(string(raw), "\x00", " "))
	return strings.Contains(cmd, "archi") || strings.Contains(cmd, "ptt")
}

type pttLock struct{ path string }

func (l *pttLock) release() {
	if l != nil && l.path != "" {
		_ = os.Remove(l.path)
	}
}

// acquirePTTLock — O_EXCL-захват. Живой владелец → SIGUSR1 («финализируйся
// или замолчи») и ErrPTTSignaled; мёртвый lock (краш, logout) — подбираем.
func acquirePTTLock() (*pttLock, error) {
	path := pttLockPath()
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, werr := fmt.Fprintf(f, "%d", os.Getpid())
			_ = f.Close()
			if werr != nil {
				_ = os.Remove(path)
				return nil, fmt.Errorf("ptt: lock write: %w", werr)
			}
			return &pttLock{path: path}, nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil, fmt.Errorf("ptt: lock %s: %w", path, err)
		}
		pid, perr := strconv.Atoi(strings.TrimSpace(string(raw)))
		if perr == nil && pttProcAlive(pid) {
			_ = syscall.Kill(pid, syscall.SIGUSR1) // «стоп» живой сессии
			return nil, ErrPTTSignaled
		}
		// протухший (краш прошлого запуска) — убрать и занять заново
		_ = os.Remove(path)
	}
	return nil, fmt.Errorf("ptt: не удалось занять %s — повтори нажатие", path)
}

// ---- запись ----

// recordPTT пишет PCM с нажатия до: хвоста тишины после речи (VAD), повторного
// нажатия (manualStop), лимита длительности; отмена — через ctx.
func recordPTT(ctx context.Context, cfg config.Config, manualStop <-chan os.Signal, log *slog.Logger) ([]byte, error) {
	rate := cfg.Voice.Mic.SampleRate
	if rate == 0 {
		rate = 16000
	}
	frameMs := 30
	frameBytes := rate / 1000 * frameMs * 2

	rec, err := startStreamRecorder(cfg.Voice.Mic, rate)
	if err != nil {
		return nil, err
	}
	defer rec.stop()

	frames := make(chan []byte, 128)
	streamErr := make(chan error, 1)
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

	noSpeech := cfg.PTT.NoSpeechTimeout.Duration
	if noSpeech <= 0 {
		noSpeech = 10 * time.Second
	}
	hardStop := cfg.PTT.MaxUtterance.Duration
	if hardStop <= 0 {
		hardStop = 20 * time.Second
	}

	v := newPTTVAD(cfg.PTT)
	started := time.Now()
	var pcm []byte
	for {
		select {
		case <-ctx.Done():
			return nil, errors.New("запись отменена")
		case <-manualStop:
			log.Info("ptt: стоп по повторному нажатию", "ms", time.Since(started).Milliseconds())
			return pcm, nil
		case frame, ok := <-frames:
			if !ok {
				select {
				case err := <-streamErr:
					return nil, err
				default:
					return nil, errors.New("ptt: поток микрофона оборвался")
				}
			}
			pcm = append(pcm, frame...)
			if v.feed(rms(frame)) {
				log.Info("ptt: автостоп по паузе",
					"speech_started", v.SpeechStarted(),
					"floor", math.Round(v.floor),
					"ms", time.Since(started).Milliseconds())
				return pcm, nil
			}
			if !v.SpeechStarted() && time.Since(started) > noSpeech {
				return nil, fmt.Errorf("речь не услышана за %.0f с — микрофон в порядке? (archi ear --test)",
					noSpeech.Seconds())
			}
			if time.Since(started) > hardStop {
				log.Warn("ptt: достигнут лимит длительности, режу запись")
				return pcm, nil
			}
		}
	}
}

// ---- главный конвейер ----

// RunPTT — один цикл push-to-talk: бип → запись (VAD-автостоп / повторное
// нажатие) → STT → агент в голосовом режиме («РЕЖИМ УШЕЙ») → озвучка.
// Контекст отменяется Ctrl+C / SIGTERM (тихая отмена без обработки фразы).
// Возврат ErrPTTSignaled означает: этот запуск лишь остановил живую сессию.
func RunPTT(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	if !cfg.Voice.STT.Enabled {
		return errors.New("voice.stt.enabled=false — включи распознавание в конфиге")
	}

	// SIGUSR1 = «повторное нажатие»: сначала канал, потом lock — иначе
	// сигнал между lock-захватом и Notify потеряется.
	usr1 := make(chan os.Signal, 1)
	signal.Notify(usr1, syscall.SIGUSR1)
	defer signal.Stop(usr1)

	lock, err := acquirePTTLock()
	if err != nil {
		return err
	}
	defer lock.release()

	// Производный контекст: отмена (Ctrl+C) гасит всё мягко.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	beep := cfg.PTT.Beep == nil || *cfg.PTT.Beep
	rate := cfg.Voice.Mic.SampleRate
	if rate == 0 {
		rate = 16000
	}

	if beep {
		playBeep(ctx, beepStart, log)
	}
	fmt.Println("🔴 Слушаю… (замолчи — сам остановлю; повторное нажатие — стоп)")

	t0 := time.Now()
	pcm, err := recordPTT(ctx, cfg, usr1, log)
	if err != nil {
		if ctx.Err() != nil {
			return nil // отмена (Ctrl+C) — тихо, без бипа
		}
		if beep {
			playBeep(context.Background(), beepError, log)
		}
		return err
	}
	if len(pcm) < rate/4*2 { // < 250 мс — фразы не было
		if beep {
			playBeep(context.Background(), beepError, log)
		}
		return errors.New("слишком короткая запись — скажи что-нибудь подольше")
	}

	// Запись закончена — с этого момента повторное нажатие («замолчи»)
	// гасит STT/LLM/озвучку через отмену контекста. До записи usr1 принадлежал
	// циклу записи (один потребитель в каждый момент — семантика однозначна).
	aborted := make(chan struct{})
	defer close(aborted)
	go func() {
		select {
		case <-usr1:
			log.Info("ptt: прерываю ответ повторным нажатием")
			cancel()
		case <-aborted:
		}
	}()

	if beep {
		playBeep(context.Background(), beepStop, log)
	}
	fmt.Printf("⏳ Распознаю… (%.1f с записи)\n", time.Since(t0).Seconds())

	// 1) STT.
	stt := NewTranscriber(cfg.Voice.STT, log)
	transcript, err := stt.Transcribe(ctx, wrapWAV(pcm, rate), "ptt.wav")
	if err != nil {
		if ctx.Err() != nil {
			return nil // прервано повторным нажатием — тишина
		}
		if beep {
			playBeep(context.Background(), beepError, log)
		}
		return fmt.Errorf("распознавание: %w", err)
	}
	transcript = strings.TrimSpace(transcript)
	if transcript == "" {
		if beep {
			playBeep(context.Background(), beepError, log)
		}
		return errors.New("речь не распознана — попробуй ещё раз")
	}
	// Привычка сильнее кнопки: «Арчи, сделай X» → «сделай X».
	if rest, m := splitWakeWord(transcript, cfg.Ear.WakeWords); m && strings.TrimSpace(rest) != "" {
		transcript = strings.TrimSpace(rest)
	}
	fmt.Printf("🗣 Вы сказали: %s\n", transcript)

	// 2) Агент в голосовом режиме (флаг voice → «РЕЖИМ УШЕЙ», сессия ear).
	socket := cfg.IPC.SocketPath
	if !cfg.IPC.Enabled || socket == "" {
		socket = "/run/archi/archi.sock"
	}
	tLLM := time.Now()
	reply, err := askAgent(ctx, socket, transcript)
	if err != nil {
		if ctx.Err() != nil {
			return nil // прервано повторным нажатием — тишина
		}
		if beep {
			playBeep(context.Background(), beepError, log)
		}
		return fmt.Errorf("агент: %w (archi serve жив?)", err)
	}
	log.Info("ptt: ответ", "text", clipE(reply, 200),
		"llm_ms", time.Since(tLLM).Milliseconds(),
		"total_ms", time.Since(t0).Milliseconds())

	// 3) Озвучка (стриминг Piper, как в ear); без TTS — текст в консоль.
	if !cfg.Voice.TTS.Enabled {
		fmt.Println(reply)
		return nil
	}
	tts := NewSynthesizer(cfg.Voice.TTS, log)
	if err := tts.Speak(ctx, reply); err != nil {
		log.Warn("ptt: озвучка не вышла (ответ в консоли)", "err", err)
		fmt.Println(reply)
	}
	return nil
}
