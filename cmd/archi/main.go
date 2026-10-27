// Archi 2026 — гибридный DevOps-агент: живёт на ноутбуке, управляет двумя нодами.
//
//	archi serve           — запуск демона (bridge + agent + telegram + ipc)
//	archi ctl "текст"     — запрос к агенту из локального терминала
//	archi ctl --status    — быстрая сводка телеметрии
//	archi ctl --reset     — сброс IPC-сессии
//	archi voice [--speak] — голосовой запрос с микрофона ноутбука
//	archi check-config    — валидация config.yaml
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	// Встроенная база таймзон: агент не зависит от /usr/share/zoneinfo на хосте.
	_ "time/tzdata"

	"github.com/archi/archi/internal/agent"
	"github.com/archi/archi/internal/bridge"
	"github.com/archi/archi/internal/config"
	"github.com/archi/archi/internal/cron"
	"github.com/archi/archi/internal/ipc"
	"github.com/archi/archi/internal/metrics"
	"github.com/archi/archi/internal/skills"
	"github.com/archi/archi/internal/telegram"
	"github.com/archi/archi/internal/tools"
	"github.com/archi/archi/internal/voice"
)

var version = "2026.14.0"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "serve":
			if err := runServe(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "archi:", err)
				os.Exit(1)
			}
			return
		case "ctl":
			if err := runCtl(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "archi-ctl:", err)
				os.Exit(1)
			}
			return
		case "voice":
			if err := runVoice(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "archi-voice:", err)
				os.Exit(1)
			}
			return
		case "ear":
			if err := runEarDaemon(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "archi-ear:", err)
				os.Exit(1)
			}
			return
		case "version":
			fmt.Println("archi", version)
			return
		case "check-config":
			if err := runCheckConfig(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "archi check-config:", err)
				os.Exit(1)
			}
			return
		}
	}
	fmt.Fprintf(os.Stderr, "Archi %s — гибридный DevOps-агент\n\nИспользование:\n  archi serve [--config config.yaml]\n  archi ctl \"текст\" [--config config.yaml]\n  archi voice [--speak] [--config config.yaml]\n  archi ear [--test|--train-wake] [--samples N] [--config config.yaml]\n      — hands-free слушатель (wake-word); --test — диагностика слуха\n  archi check-config [--config config.yaml]\n  archi version\n", version)
	os.Exit(2)
}

// flagsConfig парсит --config.
func flagsConfig(args []string) (string, error) {
	fs := flag.NewFlagSet("", flag.ContinueOnError)
	cfgPath := fs.String("config", "/etc/archi/config.yaml", "путь к config.yaml")
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	return *cfgPath, nil
}

// newLogger — slog в JSON в stdout (systemd дружит с этим).
func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lv}))
}

// runCheckConfig — валидация конфига без запуска демона.
func runCheckConfig(args []string) error {
	cfgPath, err := flagsConfig(args)
	if err != nil {
		return err
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	fmt.Printf("OK: %s валиден\n  узел: %s | таймзона: %s\n  LLM: %s (модель %s)\n  remote: %s@%s:%d\n  telegram: %v | ipc: %v (%s)\n  память: %s\n",
		cfgPath, cfg.NodeName, cfg.Timezone, cfg.LLM.BaseURL, cfg.LLM.Model,
		cfg.Bridge.Remote.User, cfg.Bridge.Remote.Host, cfg.Bridge.Remote.Port,
		cfg.Telegram.Enabled, cfg.IPC.Enabled, cfg.IPC.SocketPath, cfg.Memory.Dir)
	return nil
}

func runServe(args []string) error {
	cfgPath, err := flagsConfig(args)
	if err != nil {
		return err
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogLevel)

	// Graceful shutdown: SIGINT/SIGTERM → контекст отменяется, всё останавливается по-честному.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	tz, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		tz = time.UTC
		log.Warn("timezone не загрузилась, работаю в UTC", "tz", cfg.Timezone, "err", err)
	}

	// 1) Память (+ опциональные эмбеддинги для семантического recall).
	mem, err := agent.NewMemory(cfg.Memory.Dir, cfg.Memory.MaxEntries, log)
	if err != nil {
		return err
	}
	if cfg.Memory.Embeddings.Enabled {
		mem.SetEmbedder(agent.NewHTTPEmbedder(cfg.Memory.Embeddings))
		log.Info("memory: семантический recall включён", "model", cfg.Memory.Embeddings.Model)
	}

	// Навыки: каталог само-пишущихся процедур (memory/skills/*.md).
	var skl *skills.Skills
	if cfg.Skills.Enabled == nil || *cfg.Skills.Enabled {
		skl = skills.New(mem.SkillsDir())
		log.Info("skills: каталог включён", "dir", mem.SkillsDir(), "inject_limit", cfg.Skills.InjectLimit)
	}

	// 2) Мост (телеметрия local+remote, SSH-пул, алерты, watchdog).
	br := bridge.New(cfg.Bridge, cfg.Alerts, log)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error("bridge: фатальная паника цикла (перезапуск через 30 c)", "panic", r)
				time.Sleep(30 * time.Second)
				go br.Run(ctx)
			}
		}()
		br.Run(ctx)
	}()

	// 3) Инструменты + агент.
	registry := tools.NewRegistry(tools.Deps{Cfg: *cfg, Bridge: br, Memory: mem, Skills: skl, Log: log})
	llm := agent.NewClient(cfg.LLM, log)
	ag := agent.New(cfg.Agent, llm, registry, mem, br.Snapshot, tz, log)
	if skl != nil {
		ag.SetSkills(skl, cfg.Skills.InjectLimit)
	}
	// Субагент: делегирование разведок с чистым контекстом (delegate_task).
	registry.SetSubAgent(ag)

	// 4) IPC: UNIX-сокет.
	if cfg.IPC.Enabled {
		srv := ipc.NewServer(cfg.IPC.SocketPath, ag, br, log)
		go func() {
			if err := srv.Serve(ctx); err != nil {
				log.Error("ipc: сервер остановлен", "err", err)
			}
		}()
		defer srv.Close()
	}

	// 5) Telegram: polling + голосовые + тревоги + сводки + approval-клавиатуры.
	var runner *telegram.Runner
	if cfg.Telegram.Enabled {
		bot := telegram.New(cfg.Telegram, log)
		var stt *voice.Transcriber
		var tts *voice.Synthesizer
		if cfg.Voice.STT.Enabled {
			stt = voice.NewTranscriber(cfg.Voice.STT, log)
			log.Info("voice: STT включён", "model", cfg.Voice.STT.Model, "base_url", cfg.Voice.STT.BaseURL)
			if cfg.Voice.STT.Fallback != nil {
				log.Info("voice: STT фолбэк подключён", "model", cfg.Voice.STT.Fallback.Model,
					"base_url", cfg.Voice.STT.Fallback.BaseURL)
			}
		}
		if cfg.Voice.TTS.Enabled {
			tts = voice.NewSynthesizer(cfg.Voice.TTS, log)
			log.Info("voice: TTS включён", "model", cfg.Voice.TTS.Model, "voice", cfg.Voice.TTS.Voice)
		}
		digestChats := cfg.Telegram.Digest.Chats
		if len(digestChats) == 0 {
			digestChats = cfg.Telegram.Admins
		}
		runner = telegram.NewRunner(bot, ag, br, tz, telegram.DigestSchedule{
			Enabled: cfg.Telegram.Digest.Enabled,
			Times:   cfg.Telegram.Digest.Times,
			Chats:   digestChats,
		}, stt, tts, log)
		go runner.Run(ctx)
		log.Info("telegram: включён", "admins", len(cfg.Telegram.Admins))
	} else {
		log.Info("telegram: отключён в конфиге")
	}

	// Approval: опасные команды идут через Telegram-клавиатуру владельца.
	// Telegram выключен → подтвержать некому — политика падает в отказ (fail-closed).
	if runner != nil {
		registry.SetApprover(runner)
		log.Info("tools: approval-клавиатуры подключены")
	} else {
		log.Warn("tools: Telegram выключен — опасные команды будут ОТКЛОНЯТЬСЯ (fail-closed)")
	}

	// 6) Cron: проактивные задания агента (расписание + промпт → ReAct).
	if cfg.Cron.Enabled {
		var ntf cron.Notifier
		if runner != nil {
			ntf = runner
		}
		eng := cron.New(cfg.Cron, ag, ntf, mem, tz, log)
		go eng.Run(ctx)
		log.Info("cron: движок проактивности запущен", "jobs", len(cfg.Cron.Jobs))
	}

	// 7) Observability: Prometheus /metrics + /healthz (только localhost).
	if cfg.Observability.Enabled == nil || *cfg.Observability.Enabled {
		go func() {
			if err := metrics.Serve(ctx, cfg.Observability.MetricsAddr, version, log); err != nil {
				log.Error("metrics: эндпоинт остановлен", "err", err)
			}
		}()
	}

	log.Info("archi запущен", "version", version, "node", cfg.NodeName, "remote", cfg.Bridge.Remote.Host, "pid", os.Getpid())

	// Главный поток ждёт сигнала и следит за дочерними через контекст.
	<-ctx.Done()
	log.Info("archi: получен сигнал остановки, завершаюсь…")
	// Даём горутинам 3 секунды на дописывание данных.
	time.Sleep(3 * time.Second)
	return nil
}

func runCtl(args []string) error {
	var (
		text   string
		reset  bool
		status bool
	)
	// --config может стоять в любом месте (до или после текста запроса) —
	// вытаскиваем вручную, т.к. flag.Parse останавливается на первом позиционном.
	cfgPath := "/etc/archi/config.yaml"
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--config" || a == "-config":
			if i+1 < len(args) {
				cfgPath = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--config="):
			cfgPath = strings.TrimPrefix(a, "--config=")
		default:
			rest = append(rest, a)
		}
	}
	fs := flag.NewFlagSet("ctl", flag.ContinueOnError)
	fs.StringVar(&text, "t", "", "текст запроса (или позиционный аргумент)")
	fs.BoolVar(&reset, "reset", false, "сбросить контекст IPC-сессии")
	fs.BoolVar(&status, "status", false, "показать статус узлов")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if text == "" {
		argsLeft := fs.Args()
		if len(argsLeft) > 0 {
			text = strings.Join(argsLeft, " ")
		}
	}
	if !reset && !status && text == "" {
		return errors.New("укажи текст запроса, --status или --reset")
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		// для ctl достаточно сокета по умолчанию, если конфиг недоступен
		cfg = &config.Config{}
		cfg.IPC.SocketPath = "/run/archi/archi.sock"
	}
	_, err = ipc.RunCLI(cfg.IPC.SocketPath, text, reset, status)
	return err
}

// runVoice — archi voice: микрофон → STT → агент (через IPC) → ответ в консоль.
// Флаг --speak озвучивает ответ через TTS (если включён в конфиге).
func runVoice(args []string) error {
	var speak bool
	cfgPath := "/etc/archi/config.yaml"
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--config" || a == "-config":
			if i+1 < len(args) {
				cfgPath = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--config="):
			cfgPath = strings.TrimPrefix(a, "--config=")
		case a == "--speak":
			speak = true
		}
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if !cfg.Voice.STT.Enabled {
		return errors.New("voice.stt.enabled=false — включи распознавание в конфиге")
	}
	// Текстовый логгер в stderr: stdout оставляем для диалога с пользователем.
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 1) Запись с микрофона (push-to-talk: Enter — стоп, Ctrl+C — отмена).
	wav, err := voice.RecordInteractive(ctx, cfg.Voice.Mic, log)
	if err != nil {
		return err
	}

	// 2) Распознавание.
	fmt.Println("⏳ Распознаю…")
	stt := voice.NewTranscriber(cfg.Voice.STT, log)
	transcript, err := stt.Transcribe(ctx, wav, "mic.wav")
	if err != nil {
		return fmt.Errorf("распознавание: %w", err)
	}
	if transcript == "" {
		return errors.New("речь не распознана — попробуй записать ещё раз")
	}
	fmt.Printf("🗣 Вы сказали: %s\n\n", transcript)

	// 3) Запрос к агенту через IPC демона (сессия отдельная от Telegram).
	socket := cfg.IPC.SocketPath
	if !cfg.IPC.Enabled || socket == "" {
		socket = "/run/archi/archi.sock"
	}
	reply, err := ipc.RunCLI(socket, transcript, false, false)
	if err != nil {
		return err
	}

	// 4) Опциональная озвучка ответа.
	if speak {
		if !cfg.Voice.TTS.Enabled {
			return errors.New("--speak: voice.tts.enabled=false в конфиге")
		}
		fmt.Println("🔊 Озвучиваю ответ…")
		tts := voice.NewSynthesizer(cfg.Voice.TTS, log)
		audio, err := tts.Synthesize(ctx, voice.PlainText(reply))
		if err != nil {
			return fmt.Errorf("озвучка: %w", err)
		}
		if err := voice.Play(ctx, audio, "."+cfg.Voice.TTS.Format, log); err != nil {
			return err
		}
	}
	return nil
}

// runEarDaemon — archi ear: hands-free слушатель «Арчи, …» (отдельный процесс,
// deploy/archi-ear.service). Слушает микрофон, ловит wake-word, шлёт команды
// демону через IPC и озвучивает ответы (если включён TTS).
//
// --test       — диагностика слуха (захват → VAD → STT → wake → IPC) без демона.
// --train-wake — записать шаблоны wake-word голосом владельца (для wake_engine: dtw).
func runEarDaemon(args []string) error {
	cfgPath := "/etc/archi/config.yaml"
	var test, train bool
	samples := 5
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--config" || a == "-config":
			if i+1 < len(args) {
				cfgPath = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--config="):
			cfgPath = strings.TrimPrefix(a, "--config=")
		case a == "--test":
			test = true
		case a == "--train-wake":
			train = true
		case a == "--samples":
			if i+1 < len(args) {
				if n, err := strconv.Atoi(args[i+1]); err == nil && n > 0 && n <= 20 {
					samples = n
				}
				i++
			}
		case strings.HasPrefix(a, "--samples="):
			if n, err := strconv.Atoi(strings.TrimPrefix(a, "--samples=")); err == nil && n > 0 && n <= 20 {
				samples = n
			}
		}
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if !test && !train && !cfg.Ear.Enabled {
		return errors.New("ear.enabled=false — включи hands-free режим в конфиге")
	}
	log := newLogger(cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if test {
		return voice.RunEarTest(ctx, *cfg, log)
	}
	if train {
		return voice.RunEarTrain(ctx, *cfg, log, samples)
	}

	log.Info("archi-ear запускается", "version", version,
		"ipc", cfg.IPC.SocketPath, "wake_words", cfg.Ear.WakeWords,
		"engine", cfg.Ear.WakeEngine)
	return voice.RunEar(ctx, *cfg, log)
}
