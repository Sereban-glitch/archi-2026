// Package cron — проактивные задания агента: расписание + промпт → ReAct-цикл.
//
// Паттерн 2026 (Hermes cron-agent, OpenClaw GatewayScheduler, Amp automations):
// агент перестаёт быть чисто реактивным — он сам просыпается по расписанию,
// выполняет задачу и отчитывается владельцу. Ключевые особенности:
//   - сессия cron:<name> — у каждого задания свой контекст (TTL как у всех);
//   - failure-nudge: до 3 попыток с backoff 5m (как у Hermes), потом алерт;
//   - слоты times ["09:05"] считаются на сегодня И завтра — 22:00-вечер не
//     съедает завтрашнее утро (баг digestLoop, исправленный там же);
//   - catch-up по run_on_start: разовый прогон при старте демона (после 1 мин).
package cron

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/archi/archi/internal/config"
)

// AgentRunner — минимальный контракт агента (реализуется *agent.Agent).
type AgentRunner interface {
	Handle(ctx context.Context, sessionID, userText string) (string, error)
}

// Notifier — доставка результатов владельцу (реализуется telegram.Runner).
type Notifier interface {
	NotifyAdmins(ctx context.Context, text string)
}

// logNotifier — заглушка, когда Telegram выключен: пишем в лог.
type logNotifier struct{ log *slog.Logger }

func (l logNotifier) NotifyAdmins(ctx context.Context, text string) {
	l.log.Info("cron: результат (telegram выключен)", "text", clip(text, 300))
}

// Engine — планировщик заданий.
type Engine struct {
	cfg config.CronConfig
	ag  AgentRunner
	ntf Notifier
	tz  *time.Location
	log *slog.Logger
	mem memorySaver // эпизодическая память (episode), nil-safe
}

// memorySaver — сохранение эпизода в долговременную память (agent.Memory реализует).
type memorySaver interface {
	Save(kind, text string, tags ...string) error
}

// New создаёт движок. mem — опционально (nil = не пишем эпизоды).
func New(cfg config.CronConfig, ag AgentRunner, ntf Notifier, mem memorySaver, tz *time.Location, log *slog.Logger) *Engine {
	if ntf == nil {
		ntf = logNotifier{log}
	}
	if tz == nil {
		tz = time.UTC
	}
	return &Engine{cfg: cfg, ag: ag, ntf: ntf, mem: mem, tz: tz, log: log}
}

// Run — главный цикл: по горутине на задание. Блокирует до отмены ctx.
func (e *Engine) Run(ctx context.Context) {
	for i := range e.cfg.Jobs {
		job := e.cfg.Jobs[i]
		// Enabled — *bool с дефолтом true из applyDefaults; nil трактуем как true.
		if job.Enabled != nil && !*job.Enabled {
			e.log.Info("cron: задание выключено в конфиге", "name", job.Name)
			continue
		}
		if job.Name == "" || strings.TrimSpace(job.Prompt) == "" {
			e.log.Warn("cron: задание без имени/промпта пропущено", "index", i)
			continue
		}
		go e.jobLoop(ctx, job)
		e.log.Info("cron: задание запущено", "name", job.Name,
			"every", job.Every.Duration.String(), "times", fmt.Sprint(job.Times), "run_on_start", job.RunOnStart)
	}
	<-ctx.Done()
}

// jobLoop — жизненный цикл одного задания.
func (e *Engine) jobLoop(ctx context.Context, job config.CronJob) {
	if job.RunOnStart {
		select {
		case <-time.After(1 * time.Minute): // даём мосту/туннелю подняться
		case <-ctx.Done():
			return
		}
		if !e.fire(ctx, job) {
			return
		}
	}
	for {
		next, ok := NextSlot(time.Now().In(e.tz), job.Times, job.Every.Duration, e.tz)
		if !ok {
			e.log.Warn("cron: не удалось вычислить следующий запуск — сплю час", "name", job.Name)
			next = time.Now().Add(time.Hour)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(next)):
			if !e.fire(ctx, job) {
				return
			}
		}
	}
}

// fire — один запуск с failure-nudge (до 3 попыток, backoff 5m). false = ctx отменён.
func (e *Engine) fire(ctx context.Context, job config.CronJob) bool {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if attempt > 1 {
			select {
			case <-time.After(5 * time.Minute):
			case <-ctx.Done():
				return false
			}
		}
		timeout := job.Timeout.Duration
		if timeout <= 0 {
			timeout = 10 * time.Minute
		}
		jctx, cancel := context.WithTimeout(ctx, timeout)
		reply, err := e.ag.Handle(jctx, "cron:"+job.Name, job.Prompt)
		cancel()
		if err == nil {
			e.log.Info("cron: задание выполнено", "name", job.Name, "attempt", attempt, "reply_len", len(reply))
			if e.mem != nil {
				_ = e.mem.Save("episode", fmt.Sprintf("cron %s: %s", job.Name, clip(reply, 800)), "cron")
			}
			if job.Notify {
				e.ntf.NotifyAdmins(ctx, "⏰ ["+job.Name+"] "+clip(reply, 3500))
			}
			return true
		}
		lastErr = err
		e.log.Warn("cron: попытка не удалась", "name", job.Name, "attempt", attempt, "err", err)
		if ctx.Err() != nil {
			return false
		}
	}
	e.ntf.NotifyAdmins(ctx, fmt.Sprintf("❌ cron-задание %q не выполнено за 3 попытки: %v", job.Name, lastErr))
	return true // демона не останавливаем — на следующем слоте попробуем снова
}

// NextSlot — ближайший будущий момент запуска.
// times непуст → ближайший слот из сегодня/завтра; иначе now+every.
// Возвращает (момент, true) или (нуль, false), если расписание задать нельзя.
func NextSlot(now time.Time, times []string, every time.Duration, tz *time.Location) (time.Time, bool) {
	if len(times) > 0 {
		best := now.Add(25 * time.Hour) // больше максимального «до завтрашнего слота»
		found := false
		for day := 0; day < 2; day++ {
			for _, t := range times {
				var h, m int
				if _, err := fmt.Sscanf(t, "%d:%d", &h, &m); err != nil {
					continue
				}
				cand := time.Date(now.Year(), now.Month(), now.Day()+day, h, m, 0, 0, tz)
				if cand.After(now) && cand.Before(best) {
					best, found = cand, true
				}
			}
		}
		return best, found
	}
	if every > 0 {
		return now.Add(every), true
	}
	return time.Time{}, false
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
