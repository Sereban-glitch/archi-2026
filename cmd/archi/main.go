// Archi 2026 — гибридный DevOps-агент: живёт на ноутбуке, управляет двумя нодами.
//
//      archi serve           — запуск демона (bridge + agent + telegram + ipc)
//      archi ctl "текст"     — запрос к агенту из локального терминала
//      archi ctl --status    — быстрая сводка телеметрии
//      archi ctl --reset     — сброс IPC-сессии
//      archi check-config    — валидация config.yaml
package main

import (
        "context"
        "errors"
        "flag"
        "fmt"
        "log/slog"
        "os"
        "os/signal"
        "strings"
        "syscall"
        "time"
        // Встроенная база таймзон: агент не зависит от /usr/share/zoneinfo на хосте.
        _ "time/tzdata"

        "github.com/archi/archi/internal/agent"
        "github.com/archi/archi/internal/bridge"
        "github.com/archi/archi/internal/config"
        "github.com/archi/archi/internal/ipc"
        "github.com/archi/archi/internal/telegram"
        "github.com/archi/archi/internal/tools"
)

var version = "2026.10.0"

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
        fmt.Fprintf(os.Stderr, "Archi %s — гибридный DevOps-агент\n\nИспользование:\n  archi serve [--config config.yaml]\n  archi ctl \"текст\" [--config config.yaml]\n  archi check-config [--config config.yaml]\n  archi version\n", version)
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

        // 1) Память.
        mem, err := agent.NewMemory(cfg.Memory.Dir, cfg.Memory.MaxEntries, log)
        if err != nil {
                return err
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
        registry := tools.NewRegistry(tools.Deps{Cfg: *cfg, Bridge: br, Memory: mem, Log: log})
        llm := agent.NewClient(cfg.LLM, log)
        ag := agent.New(cfg.Agent, llm, registry, mem, br.Snapshot, tz, log)

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

        // 5) Telegram: polling + тревоги + сводки.
        if cfg.Telegram.Enabled {
                bot := telegram.New(cfg.Telegram, log)
                digestChats := cfg.Telegram.Digest.Chats
                if len(digestChats) == 0 {
                        digestChats = cfg.Telegram.Admins
                }
                runner := telegram.NewRunner(bot, ag, br, tz, telegram.DigestSchedule{
                        Enabled: cfg.Telegram.Digest.Enabled,
                        Times:   cfg.Telegram.Digest.Times,
                        Chats:   digestChats,
                }, log)
                go runner.Run(ctx)
                log.Info("telegram: включён", "admins", len(cfg.Telegram.Admins))
        } else {
                log.Info("telegram: отключён в конфиге")
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
        return ipc.RunCLI(cfg.IPC.SocketPath, text, reset, status)
}
