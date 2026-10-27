package bridge

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/archi/archi/internal/config"
)

// Bridge — оркестратор межмашинного моста:
//   - по тику собирает метрики ноутбука и agrobot-prod (параллельно);
//   - ведёт атомарный снапшот Status для промптов агента и IPC;
//   - оценивает пороги (Alerter) и state-машину up/down удалённой ноды;
//   - рассылает события подписчикам (Telegram-бот, IPC, лог);
//   - watchdog: автоперезапуск заданных сервисов на VM с cooldown.
type Bridge struct {
	cfg      config.BridgeConfig
	alertCfg config.AlertsConfig

	local   Collector
	remote  Collector
	exec    *Executor
	alerter *Alerter
	log     *slog.Logger

	snap     atomic.Pointer[Status]
	remoteUp bool // предыдущее известное состояние удалённой ноды

	mu        sync.Mutex
	subs      []chan Event
	restartAt map[string]time.Time // сервис → время последнего автоперезапуска
}

// New собирает мост из конфигурации.
func New(cfg config.BridgeConfig, alertCfg config.AlertsConfig, log *slog.Logger) *Bridge {
	exec := NewExecutor(cfg, log)
	b := &Bridge{
		cfg:       cfg,
		alertCfg:  alertCfg,
		exec:      exec,
		alerter:   NewAlerter(alertCfg, log),
		log:       log,
		remoteUp:  true, // оптимистично; первый цикл выставит реальное значение
		restartAt: make(map[string]time.Time),
	}
	b.local = NewLocalCollector(NodeLocal, alertCfg.Services, log)
	b.remote = NewRemoteCollector(exec, RemoteConfig{
		Timeout:  cfg.RemoteTimeout.Duration,
		Services: alertCfg.Services,
	}, log)
	return b
}

// Executor открывает доступ к исполнителю команд (нужно tools-пакету).
func (b *Bridge) Executor() Runner { return b.exec }

// Snapshot возвращает последний снимок состояния обеих нод (может быть nil до первого цикла).
func (b *Bridge) Snapshot() *Status { return b.snap.Load() }

// Subscribe подписывается на события моста (тревоги, up/down, рестарты).
// Канал буферизован; при переполнении события теряются (мост не блокируется).
func (b *Bridge) Subscribe() <-chan Event {
	ch := make(chan Event, 64)
	b.mu.Lock()
	b.subs = append(b.subs, ch)
	b.mu.Unlock()
	return ch
}

func (b *Bridge) broadcast(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		select {
		case ch <- e:
		default:
			b.log.Warn("bridge: подписчик переполнен, событие отброшено", "type", e.Type, "node", e.Node)
		}
	}
}

// Run — главный цикл моста; живёт до отмены контекста.
func (b *Bridge) Run(ctx context.Context) {
	go b.exec.ssh.keepaliveLoop(ctx)

	// Первый сбор сразу, без ожидания тика.
	b.cycle(ctx)

	t := time.NewTicker(b.cfg.Interval.Duration)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			b.exec.ssh.Close()
			return
		case <-t.C:
			b.cycle(ctx)
		}
	}
}

// cycle — один цикл телеметрии: параллельный сбор, снапшот, тревоги, watchdog.
func (b *Bridge) cycle(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			b.log.Error("bridge: panic в цикле сбора восстановлен", "panic", r)
		}
	}()

	cctx, cancel := context.WithTimeout(ctx, b.cfg.RemoteTimeout.Duration+5*time.Second)
	defer cancel()

	var (
		wg        sync.WaitGroup
		localRes  = make(chan *NodeMetrics, 1)
		remoteRes = make(chan *NodeMetrics, 1)
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		localRes <- b.local.Collect(cctx)
	}()
	go func() {
		defer wg.Done()
		remoteRes <- b.remote.Collect(cctx)
	}()
	wg.Wait()
	close(localRes)
	close(remoteRes)

	local, remote := <-localRes, <-remoteRes
	st := &Status{Local: local, Remote: remote}
	b.snap.Store(st)

	// Тревоги по обеим нодам.
	for _, e := range b.alerter.Evaluate(local) {
		e.Metrics = local
		b.broadcast(e)
	}
	for _, e := range b.alerter.Evaluate(remote) {
		e.Metrics = remote
		b.broadcast(e)
	}

	// State-машина доступности VM: down при 3 циклах подряд? Нет — сразу, но с эвентом.
	if remote.Online != b.remoteUp {
		now := time.Now()
		if remote.Online {
			e := Event{Type: EventNodeUp, Node: NodeRemote, Severity: "info", Timestamp: now,
				Message: "✅ agrobot-prod снова на связи (SSH ок)"}
			b.broadcast(e)
		} else {
			e := Event{Type: EventNodeDown, Node: NodeRemote, Severity: "crit", Timestamp: now,
				Message: "🔴 agrobot-prod недоступен по SSH: " + remote.Error}
			b.broadcast(e)
		}
		b.remoteUp = remote.Online
	}

	// Watchdog: автоперезапуск сервисов на VM (только если она доступна).
	if remote.Online {
		for _, svc := range b.cfg.Watchdog.AutoRestart {
			stSvc := remote.Service(svc)
			if stSvc == nil || (stSvc.State != "failed" && stSvc.State != "inactive") {
				continue
			}
			if t, ok := b.restartAt[svc]; ok && time.Since(t) < b.cfg.Watchdog.RestartCooldown.Duration {
				continue
			}
			b.restartAt[svc] = time.Now()
			cmd := "sudo -n systemctl restart " + shellQuote(svc)
			res, err := b.exec.Run(ctx, NodeRemote, cmd, 30*time.Second, 16*1024)
			switch {
			case err != nil:
				b.log.Error("watchdog: ошибка рестарта", "svc", svc, "err", err)
			case res.ExitCode != 0:
				b.broadcast(Event{Type: EventRestarted, Node: NodeRemote, Severity: "crit", Timestamp: time.Now(),
					Message: "⚠️ watchdog: рестарт " + svc + " не удался (exit " + itoa(res.ExitCode) + "): " + trimNL(res.Stderr)})
			default:
				b.broadcast(Event{Type: EventRestarted, Node: NodeRemote, Severity: "warn", Timestamp: time.Now(),
					Message: "🔁 watchdog перезапустил сервис " + svc + " на agrobot-prod"})
			}
		}
	}
}

func trimNL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
