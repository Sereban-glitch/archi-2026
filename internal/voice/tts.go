// tts.go — синтез речи (Text-to-Speech), релиз 2026.15 «Живой голос».
//
// Провайдеры:
//   - piper   — локальный офлайн-синтез (бинарь + .onnx-голос, deploy/setup-piper.sh).
//     Параметры живости (noise_scale/noise_w/length_scale) лечат дребезг
//     и дрожь ruslan; ear озвучивает СТРИМОМ: Piper читает stdin построчно
//     (одна фраза = строка) и гонит raw PCM прямо в плеер — первое слово
//     звучит, пока синтезируются остальные.
//   - command — любая внешняя команда: текст на stdin, аудио на stdout
//     (Edge-TTS, Silero, piper1-gpl… см. deploy/edge-tts.sh).
//   - прочее  — OpenAI-совместимый POST {base_url}/audio/speech (облако).
//
// Весь текст проходит NormalizeForSpeech: «2.56» → «2 и 56», «GiB» → «гигабайт»,
// «76%» → «76 процентов», markdown/эмодзи/URL вычищаются.
package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/archi/archi/internal/config"
)

// Synthesizer — клиент TTS. nil-safe: везде в коде проверяется на nil.
type Synthesizer struct {
	cfg  config.VoiceTTSConfig
	log  *slog.Logger
	http *http.Client

	piperOnce sync.Once
	piperRate int // частота дискретизации голоса (из .onnx.json), для raw-стрима
}

// NewSynthesizer создаёт синтезатор.
func NewSynthesizer(cfg config.VoiceTTSConfig, log *slog.Logger) *Synthesizer {
	timeout := cfg.Timeout.Duration
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &Synthesizer{cfg: cfg, log: log, http: &http.Client{Timeout: timeout}}
}

// MaxChars — лимит длины текста для озвучки (0 = без лимита).
func (s *Synthesizer) MaxChars() int {
	if s == nil {
		return 0
	}
	return s.cfg.MaxChars
}

// PlainText подгоняет ответ агента под озвучку.
// С 2026.15 — полное развёртывание (числа, единицы, markdown): см. NormalizeForSpeech.
func PlainText(s string) string { return NormalizeForSpeech(s) }

// Synthesize превращает текст в аудио-байты. Текст нормализуется под речь
// (идемпотентно — двойной вызов не портит результат).
func (s *Synthesizer) Synthesize(ctx context.Context, text string) ([]byte, error) {
	if s == nil {
		return nil, errors.New("tts отключён")
	}
	text = strings.TrimSpace(NormalizeForSpeech(text))
	if text == "" {
		return nil, errors.New("пустой текст")
	}
	provider := strings.ToLower(strings.TrimSpace(s.cfg.Provider))
	switch provider {
	case "piper":
		return s.synthesizePiper(ctx, text)
	case "command":
		return s.synthesizeCommand(ctx, text)
	default:
		return s.synthesizeHTTP(ctx, text)
	}
}

// Speak — озвучить текст вслух на ноутбуке с минимальной задержкой.
// Piper: стриминг raw PCM в плеер (первая фраза звучит раньше готовности всего
// текста). Остальные провайдеры: Synthesize → Play через temp-файл.
func (s *Synthesizer) Speak(ctx context.Context, text string) error {
	if s == nil {
		return errors.New("tts отключён")
	}
	text = strings.TrimSpace(NormalizeForSpeech(text))
	if text == "" {
		return nil
	}
	if strings.EqualFold(strings.TrimSpace(s.cfg.Provider), "piper") && s.cfg.Piper.Bin != "" {
		if err := s.speakPiperStream(ctx, text); err == nil {
			return nil
		} else {
			s.log.Warn("tts: потоковая озвучка не вышла, откат на файловую", "err", err)
		}
	}
	audio, err := s.Synthesize(ctx, text)
	if err != nil {
		return err
	}
	return Play(ctx, audio, playExt(s.cfg.Format), s.log)
}

// ---- piper ----

// piperTuningArgs — флаги «живого голоса»: чистота против дребезга и темп.
// Значения из конфига (дефолты задаёт applyDefaults: 0.333 / 0.6 / 0.95 —
// сильная шумность piper 0.667/0.8 «дрожит» на согласных).
func piperTuningArgs(p config.PiperConfig) []string {
	var args []string
	if p.NoiseScale != 0 {
		args = append(args, "--noise_scale", strconv.FormatFloat(p.NoiseScale, 'f', -1, 64))
	}
	if p.NoiseW != 0 {
		args = append(args, "--noise_w", strconv.FormatFloat(p.NoiseW, 'f', -1, 64))
	}
	if p.LengthScale != 0 {
		args = append(args, "--length_scale", strconv.FormatFloat(p.LengthScale, 'f', -1, 64))
	}
	return args
}

// piperSampleRate — частота дискретизации голоса из .onnx.json (кэш на клиенте).
// Стриминг --output_raw идёт без заголовков — частоту нужно знать заранее.
func (s *Synthesizer) piperSampleRate() int {
	s.piperOnce.Do(func() {
		if s.cfg.Piper.SampleRate > 0 {
			s.piperRate = s.cfg.Piper.SampleRate
			return
		}
		s.piperRate = 22050 // все ru_RU-medium голоса piper — 22050; правим, если json скажет иное
		if s.cfg.Piper.Model != "" {
			raw, err := os.ReadFile(s.cfg.Piper.Model + ".json")
			if err != nil {
				return
			}
			var meta struct {
				Audio struct {
					SampleRate int `json:"sample_rate"`
				} `json:"audio"`
			}
			if json.Unmarshal(raw, &meta) == nil && meta.Audio.SampleRate > 0 {
				s.piperRate = meta.Audio.SampleRate
			}
		}
	})
	return s.piperRate
}

// speakPiperStream — потоковая озвучка: piper (stdin: фразы построчно,
// stdout: raw s16le PCM) | плеер (ffplay/aplay/pacat). Два процесса в своих
// группах: отмена ctx гасит оба (barge-in). Прерывания игрока рвут pipe —
// piper добивается SIGPIPE и не висит.
func (s *Synthesizer) speakPiperStream(ctx context.Context, text string) error {
	rate := s.piperSampleRate()

	timeout := s.cfg.Piper.Timeout.Duration
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, 2*timeout) // стрим длинного текста — длиннее одиночного синтеза
	defer cancel()

	piper := exec.CommandContext(cctx, s.cfg.Piper.Bin,
		append([]string{"--model", s.cfg.Piper.Model, "--output_raw"}, piperTuningArgs(s.cfg.Piper)...)...)
	piper.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	piper.Stdin = strings.NewReader(strings.Join(SpeechChunks(text, 200), "\n"))
	var perr bytes.Buffer
	piper.Stderr = &perr

	pout, err := piper.StdoutPipe()
	if err != nil {
		return fmt.Errorf("piper stdout: %w", err)
	}
	if err := piper.Start(); err != nil {
		return fmt.Errorf("piper старт: %w", err)
	}

	type playerCmd struct {
		bin  string
		args []string
	}
	var candidates []playerCmd
	if p, err := lookCommand("ffplay"); err == nil {
		candidates = append(candidates, playerCmd{p, []string{
			"-autoexit", "-nodisp", "-loglevel", "quiet",
			"-f", "s16le", "-ar", strconv.Itoa(rate), "-ac", "1", "-"}})
	}
	if p, err := lookCommand("pacat"); err == nil { // PipeWire/Pulse-сессия (юнит пользователя)
		candidates = append(candidates, playerCmd{p, []string{
			"--playback", "--raw",
			"--rate", strconv.Itoa(rate), "--format=s16le", "--channels=1"}})
	}
	if p, err := lookCommand("aplay"); err == nil {
		candidates = append(candidates, playerCmd{p, []string{
			"-q", "-f", "S16_LE", "-r", strconv.Itoa(rate), "-c", "1", "-"}})
	}
	if len(candidates) == 0 {
		_ = syscall.Kill(-piper.Process.Pid, syscall.SIGKILL)
		_, _ = piper.Process.Wait()
		return errors.New("нет плеера для raw-стрима (ffplay/pacat/aplay)")
	}

	var playErr error
	for _, pl := range candidates {
		player := exec.CommandContext(cctx, pl.bin, pl.args...)
		player.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		player.Stdin = pout
		var lerr bytes.Buffer
		player.Stderr = &lerr
		if err := player.Start(); err != nil {
			playErr = fmt.Errorf("плеер %s: %w", pl.bin, err)
			continue // плеер не стартовал — пайп цел, пробуем следующий
		}
		// Оба процесса живы: ждём piper (источник), затем игрока.
		perrWait := piper.Wait()
		playerErr := player.Wait()
		switch {
		case playerErr == nil:
			return perrWait // доиграло — ошибка piper существеннее молчания о ней
		case cctx.Err() != nil:
			return perrWait // нас отменили (barge-in) — это не поломка стрима
		default:
			// Плеер сдох на середине потока: аудио потеряно, пайп пуст —
			// следующий плеер нечего читать; честно отдаём ошибку наверх,
			// ear откатится на файловый путь озвучки.
			if msg := strings.TrimSpace(perr.String()); msg != "" {
				s.log.Warn("tts: piper stderr при сбое плеера", "msg", clipE(msg, 200))
			}
			return fmt.Errorf("плеер %s: %w%s", pl.bin, playerErr, suffixMsg(clipB(lerr.Bytes(), 200)))
		}
	}
	if playErr != nil {
		return playErr
	}
	return errors.New("tts: ни один плеер не стартовал")
}

// synthesizePiper — локальный синтез в файл: текст на stdin бинарю, WAV в temp-файл,
// затем конвертация под целевой формат (Telegram любит opus).
func (s *Synthesizer) synthesizePiper(ctx context.Context, text string) ([]byte, error) {
	timeout := s.cfg.Piper.Timeout.Duration
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	tmp, err := writeTemp(nil, ".wav")
	if err != nil {
		return nil, err
	}
	defer removeTemp(tmp)

	args := append([]string{"--model", s.cfg.Piper.Model, "--output_file", tmp}, piperTuningArgs(s.cfg.Piper)...)
	cmd := exec.CommandContext(cctx, s.cfg.Piper.Bin, args...)
	cmd.Stdin = strings.NewReader(text)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("piper: %w%s", err, suffixMsg(clipB(stderr.Bytes(), 200)))
	}
	wav, err := os.ReadFile(tmp)
	if err != nil {
		return nil, fmt.Errorf("piper: чтение результата: %w", err)
	}
	if len(wav) < 44 {
		return nil, errors.New("piper: пустой результат")
	}
	fmt := strings.ToLower(s.cfg.Format)
	if fmt == "" || fmt == "wav" {
		return wav, nil
	}
	// Целевой формат — конвертируем (без ffmpeg отдаём WAV как есть).
	out, cerr := ffmpegConvertAudio(cctx, wav, "."+fmt)
	if cerr != nil {
		s.log.Warn("tts: piper-конвертация не удалась, отдаю WAV", "err", cerr)
		return wav, nil
	}
	return out, nil
}

// ---- command-провайдер: любой внешний движок ----

// synthesizeCommand — внешний TTS: текст на stdin, аудио-байты с stdout.
// Формат обязан соответствовать voice.tts.format (opus/mp3/wav…).
// Так подключаются Edge-TTS (deploy/edge-tts.sh), Silero, piper1-gpl и всё,
// что умеет читать pipe — без вендорлока на проприетарные протоколы.
func (s *Synthesizer) synthesizeCommand(ctx context.Context, text string) ([]byte, error) {
	if s.cfg.Command.Bin == "" {
		return nil, errors.New("tts command: не задан bin (voice.tts.command.bin)")
	}
	timeout := s.cfg.Command.Timeout.Duration
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, s.cfg.Command.Bin, s.cfg.Command.Args...)
	cmd.Stdin = strings.NewReader(text)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("tts command %s: %w%s", s.cfg.Command.Bin, err, suffixMsg(clipB(stderr.Bytes(), 200)))
	}
	if stdout.Len() == 0 {
		return nil, errors.New("tts command: пустой stdout")
	}
	return stdout.Bytes(), nil
}

// ffmpegConvertAudio конвертирует аудио-байты в целевой формат через ffmpeg.
func ffmpegConvertAudio(ctx context.Context, audio []byte, ext string) ([]byte, error) {
	if _, err := lookCommand("ffmpeg"); err != nil {
		return nil, errors.New("ffmpeg не установлен")
	}
	src, err := writeTemp(audio, ".wav")
	if err != nil {
		return nil, err
	}
	defer removeTemp(src)
	dst := strings.TrimSuffix(src, ".wav") + ext
	var codec string
	switch strings.ToLower(ext) {
	case ".opus", ".oga", ".ogg":
		codec = "libopus"
	case ".mp3":
		codec = "libmp3lame"
	case ".aac", ".m4a":
		codec = "aac"
	case ".flac":
		codec = "flac"
	case ".raw", ".pcm":
		codec = "pcm_s16le"
	default:
		return nil, fmt.Errorf("неподдерживаемый формат конвертации %q", ext)
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-i", src, "-c:a", codec, "-b:a", "48k", dst)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		_ = os.Remove(dst)
		return nil, fmt.Errorf("ffmpeg: %w%s", err, suffixMsg(clipB(stderr.Bytes(), 200)))
	}
	out, err := os.ReadFile(dst)
	_ = os.Remove(dst)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// synthesizeHTTP — облачный синтез через OpenAI-совместимый /audio/speech.
func (s *Synthesizer) synthesizeHTTP(ctx context.Context, text string) ([]byte, error) {
	if s.cfg.MaxChars > 0 {
		r := []rune(text)
		if len(r) > s.cfg.MaxChars {
			text = string(r[:s.cfg.MaxChars])
		}
	}

	payload, err := json.Marshal(map[string]any{
		"model":           s.cfg.Model,
		"input":           text,
		"voice":           s.cfg.Voice,
		"response_format": s.cfg.Format,
	})
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(s.cfg.BaseURL, "/") + "/audio/speech"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("запрос: %w", err)
	}
	if s.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.cfg.APIKey)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("сеть: %w", err)
	}
	defer resp.Body.Close()

	// Успех = аудио-данные; JSON в теле — всегда ошибка провайдера.
	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/json") {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, fmt.Errorf("tts http %d: %s", resp.StatusCode, clipB(raw, 300))
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("tts http %d: %s", resp.StatusCode, clipB(raw, 300))
	}
	audio, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("чтение аудио: %w", err)
	}
	if len(audio) == 0 {
		return nil, errors.New("tts: пустой ответ")
	}
	return audio, nil
}

// Play воспроизводит аудио-байты на ноутбуке: ffplay (ffmpeg) → paplay → aplay.
// Используется командой `archi voice --speak` и файловым fallback озвучки ear.
func Play(ctx context.Context, audio []byte, ext string, log *slog.Logger) error {
	tmp, err := writeTemp(audio, ext)
	if err != nil {
		return err
	}
	defer removeTemp(tmp)

	type player struct {
		bin  string
		args []string
	}
	var candidates []player
	if p, err := lookCommand("ffplay"); err == nil {
		candidates = append(candidates, player{p, []string{"-autoexit", "-nodisp", "-loglevel", "quiet", tmp}})
	}
	if p, err := lookCommand("paplay"); err == nil {
		candidates = append(candidates, player{p, []string{tmp}})
	}
	if p, err := lookCommand("aplay"); err == nil && ext == ".wav" {
		candidates = append(candidates, player{p, []string{"-q", tmp}})
	}
	if len(candidates) == 0 {
		return errors.New("нет плеера: установи ffmpeg (ffplay) или pulseaudio-utils (paplay)")
	}

	for _, pl := range candidates {
		if err := runPlayer(ctx, pl.bin, pl.args); err == nil {
			return nil
		} else {
			log.Warn("voice: плеер не справился, пробую следующий", "bin", pl.bin, "err", err)
		}
	}
	return errors.New("ни один плеер не воспроизвёл аудио")
}
