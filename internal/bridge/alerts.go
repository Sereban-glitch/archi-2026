package bridge

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/archi/archi/internal/config"
)

// Alerter — движок пороговых тревог с дедупликацией (cooldown на пару нода+вид).
// Вдохновлено практикой продовых watchdog-ов 2026: тревога без cooldown — это спам,
// а спам приучает игнорировать действительно критичные события.
type Alerter struct {
	cfg       config.AlertsConfig
	log       *slog.Logger
	mu        sync.Mutex
	lastFired map[string]time.Time // "node|kind" → время последней тревоги
}

// NewAlerter создаёт движок тревог.
func NewAlerter(cfg config.AlertsConfig, log *slog.Logger) *Alerter {
	return &Alerter{cfg: cfg, log: log, lastFired: make(map[string]time.Time)}
}

// Evaluate проверяет метрики ноды на пробитие порогов, возвращает новые события.
func (a *Alerter) Evaluate(m *NodeMetrics) []Event {
	if !m.Online || m.Error != "" {
		return nil // down-состояния обрабатывает state-машина Bridge
	}
	var events []Event
	now := time.Now()

	// RAM
	if p := m.MemPercent(); p >= a.cfg.RAMPercent {
		if e, ok := a.fire(now, m.Node, "ram"); ok {
			e.Severity = "warn"
			e.Message = fmt.Sprintf("⚠️ RAM на %s (%s): %.1f%% (порог %.0f%%) — свободно %s",
				m.Node, m.Hostname, p, a.cfg.RAMPercent, kbHuman(m.MemAvailKB))
			events = append(events, *e)
		}
	}
	// CPU
	if m.CPUPercent >= a.cfg.CPUPercent {
		if e, ok := a.fire(now, m.Node, "cpu"); ok {
			e.Severity = "warn"
			e.Message = fmt.Sprintf("⚠️ CPU на %s (%s): %.0f%% load=%.2f (порог %.0f%%)",
				m.Node, m.Hostname, m.CPUPercent, m.Load1, a.cfg.CPUPercent)
			events = append(events, *e)
		}
	}
	// Диск
	if p := m.DiskPercent(); p >= a.cfg.DiskPercent {
		if e, ok := a.fire(now, m.Node, "disk"); ok {
			e.Severity = "crit"
			e.Message = fmt.Sprintf("🔴 Диск / на %s (%s): %.1f%% занято, свободно %s (порог %.0f%%)",
				m.Node, m.Hostname, p, kbHuman(m.DiskAvailKB), a.cfg.DiskPercent)
			events = append(events, *e)
		}
	}
	// Сервисы
	for _, s := range m.Services {
		if s.State == "failed" || s.State == "inactive" {
			if e, ok := a.fire(now, m.Node, "svc:"+s.Name); ok {
				e.Severity = "crit"
				e.Message = fmt.Sprintf("🔴 Сервис %s на %s: состояние %s", s.Name, m.Node, s.State)
				events = append(events, *e)
			}
		}
	}
	// Батарея ноутбука
	if m.Battery != nil && m.Battery.Status == "Discharging" && m.Battery.Percent <= 15 {
		if e, ok := a.fire(now, m.Node, "battery"); ok {
			e.Severity = "warn"
			e.Message = fmt.Sprintf("🔋 Батарея ноутбука: %d%% (%s), осталось ~%.1f ч",
				m.Battery.Percent, m.Battery.Status, m.Battery.EnergyNow/max(m.Battery.RateW, 0.1))
			events = append(events, *e)
		}
	}
	return events
}

// fire решает, стрелять ли тревогой (cooldown), и регистрирует момент.
func (a *Alerter) fire(now time.Time, node, kind string) (*Event, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := node + "|" + kind
	if t, ok := a.lastFired[key]; ok && now.Sub(t) < a.cfg.Cooldown.Duration {
		return nil, false
	}
	a.lastFired[key] = now
	return &Event{Type: EventAlert, Node: node, Timestamp: now}, true
}

func kbHuman(kb uint64) string {
	const k = 1024
	switch {
	case kb >= k*k:
		return fmt.Sprintf("%.1f GiB", float64(kb)/(k*k))
	case kb >= k:
		return fmt.Sprintf("%.0f MiB", float64(kb)/k)
	default:
		return fmt.Sprintf("%d KiB", kb)
	}
}

func max(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
