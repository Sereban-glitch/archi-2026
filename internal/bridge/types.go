// Package bridge — межмашинный мост Archi: телеметрия обеих нод, SSH-пул,
// удалённое исполнение команд и канал тревог.
package bridge

import (
	"context"
	"time"
)

// NodeMetrics — снимок состояния одной ноды (унифицирован для локальной и удалённой машины).
type NodeMetrics struct {
	Node      string    `json:"node"`
	Timestamp time.Time `json:"ts"`
	Online    bool      `json:"online"`
	Error     string    `json:"error,omitempty"`
	Hostname  string    `json:"hostname,omitempty"`
	Kernel    string    `json:"kernel,omitempty"`

	CPUPercent float64 `json:"cpu_percent"`
	Load1      float64 `json:"load1"`
	Load5      float64 `json:"load5"`
	Load15     float64 `json:"load15"`

	MemTotalKB  uint64 `json:"mem_total_kb"`
	MemAvailKB  uint64 `json:"mem_avail_kb"`
	SwapTotalKB uint64 `json:"swap_total_kb"`
	SwapFreeKB  uint64 `json:"swap_free_kb"`

	DiskTotalKB uint64 `json:"disk_total_kb"`
	DiskUsedKB  uint64 `json:"disk_used_kb"`
	DiskAvailKB uint64 `json:"disk_avail_kb"`

	UptimeSec uint64 `json:"uptime_sec"`

	Battery      *BatteryInfo            `json:"battery,omitempty"`
	Temperatures map[string]float64      `json:"temperatures,omitempty"`
	Services     []ServiceState          `json:"services,omitempty"`
}

// BatteryInfo — состояние батареи ноутбука (отсутствует на VM).
type BatteryInfo struct {
	Percent   int     `json:"percent"`
	Status    string  `json:"status"` // Charging / Discharging / Full / Unknown
	EnergyNow float64 `json:"energy_now_wh,omitempty"`
	EnergyMax float64 `json:"energy_max_wh,omitempty"`
	RateW     float64 `json:"rate_w,omitempty"`
}

// ServiceState — состояние systemd-юнита.
type ServiceState struct {
	Name   string `json:"name"`
	State  string `json:"state"` // active / inactive / failed / unknown
	Detail string `json:"detail,omitempty"`
}

// MemPercent — доля занятой RAM в процентах.
func (m *NodeMetrics) MemPercent() float64 {
	if m.MemTotalKB == 0 {
		return 0
	}
	used := float64(m.MemTotalKB - m.MemAvailKB)
	return 100 * used / float64(m.MemTotalKB)
}

// DiskPercent — доля занятого корневого диска в процентах.
func (m *NodeMetrics) DiskPercent() float64 {
	used, avail := m.DiskUsedKB, m.DiskAvailKB
	total := used + avail
	if total == 0 {
		return 0
	}
	return 100 * float64(used) / float64(total)
}

// Service возвращает состояние сервиса по имени (nil, если не мониторится).
func (m *NodeMetrics) Service(name string) *ServiceState {
	for i := range m.Services {
		if m.Services[i].Name == name {
			return &m.Services[i]
		}
	}
	return nil
}

// EventType — тип события моста.
type EventType string

const (
	EventAlert     EventType = "alert"       // сработал порог (RAM/CPU/disk/service)
	EventNodeDown  EventType = "node_down"   // нода перестала отвечать
	EventNodeUp    EventType = "node_up"     // нода вернулась в строй
	EventRestarted EventType = "restarted"   // watchdog перезапустил сервис
)

// Event — событие, публикуемое мостом наружу (боту, IPC, логу).
type Event struct {
	Type      EventType  `json:"type"`
	Node      string     `json:"node"`
	Message   string     `json:"message"`
	Severity  string     `json:"severity"` // warn / crit / info
	Timestamp time.Time  `json:"ts"`
	Metrics   *NodeMetrics `json:"metrics,omitempty"`
}

// Status — совокупный снимок обеих нод (пополам собирается раз в bridge.Interval).
type Status struct {
	Local  *NodeMetrics `json:"local"`
	Remote *NodeMetrics `json:"remote"`
}

// SnapshotFn — поставщик свежего снимка (реализуется *Bridge).
type SnapshotFn func() *Status

// Runner — унифицированный запускатель команд на ноде (local / remote).
type Runner interface {
	Run(ctx context.Context, node, command string, timeout time.Duration, maxOutput int) (*ExecResult, error)
}

// ExecResult — результат исполненной команды.
type ExecResult struct {
	Node     string        `json:"node"`
	Command  string        `json:"command"`
	Stdout   string        `json:"stdout"`
	Stderr   string        `json:"stderr"`
	ExitCode int           `json:"exit_code"`
	Duration time.Duration `json:"duration_ms"`
	Truncated bool         `json:"truncated"`
}
