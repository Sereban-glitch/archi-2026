package bridge

import (
	"bufio"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// NodeLocal / NodeRemote — канонические имена нод во всех инструментах и метриках.
const (
	NodeLocal  = "local"
	NodeRemote = "remote"
)

// LocalCollector — сборщик метрик ноутбука (чистый stdlib: /proc, /sys, statfs).
type LocalCollector struct {
	node     string
	services []string
	log      *slog.Logger
}

// NewLocalCollector создаёт локальный коллектор.
func NewLocalCollector(node string, services []string, log *slog.Logger) *LocalCollector {
	return &LocalCollector{node: node, services: services, log: log}
}

// Collect снимает метрики локальной машины (занимает ~1 c из-за семплирования CPU).
func (c *LocalCollector) Collect(ctx context.Context) *NodeMetrics {
	m := &NodeMetrics{
		Node:      c.node,
		Timestamp: time.Now().UTC(),
		Online:    true,
	}
	defer func() {
		if r := recover(); r != nil {
			c.log.Error("local collector: panic восстановлен", "panic", r)
			m.Error = "collector panic"
		}
	}()

	m.Hostname, _ = os.Hostname()
	m.Kernel = readTrim("/proc/sys/kernel/osrelease")
	m.Services = probeSystemd(ctx, c.services)

	b1, t1, ok1 := readCPUSample()
	select {
	case <-time.After(1 * time.Second):
	case <-ctx.Done():
		m.Error = "cancelled"
		return m
	}
	b2, t2, _ := readCPUSample()
	if ok1 && t2 > t1 {
		if d := t2 - t1; d > 0 {
			m.CPUPercent = 100 * float64(b2-b1) / float64(d)
		}
	}

	if l := readCPULoad(); l != nil {
		m.Load1, m.Load5, m.Load15 = l[0], l[1], l[2]
	}
	readMeminfo(m)

	if st, err := diskStats("/"); err == nil {
		m.DiskTotalKB, m.DiskUsedKB, m.DiskAvailKB = st[0], st[1], st[2]
	}
	if up, err := readUptime(); err == nil {
		m.UptimeSec = up
	}
	if b := readBattery(); b != nil {
		m.Battery = b
	}
	m.Temperatures = readThermal()
	return m
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// readCPUSample возвращает (busy, total, ok) агрегированной строки cpu из /proc/stat.
func readCPUSample() (busy, total uint64, ok bool) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 || fields[0] != "cpu" {
			continue
		}
		var v [10]uint64
		for i := 1; i < len(fields) && i <= 10; i++ {
			x, _ := strconv.ParseUint(fields[i], 10, 64)
			v[i-1] = x
		}
		// user nice system idle iowait irq softirq steal
		busy = v[0] + v[1] + v[2] + v[5] + v[6] + v[7] + v[8]
		total = busy + v[3] + v[4]
		return busy, total, true
	}
	return 0, 0, false
}

func readCPULoad() []float64 {
	s := readTrim("/proc/loadavg")
	f := strings.Fields(s)
	if len(f) < 3 {
		return nil
	}
	out := make([]float64, 3)
	for i := 0; i < 3; i++ {
		if v, err := strconv.ParseFloat(f[i], 64); err == nil {
			out[i] = v
		}
	}
	return out
}

func readMeminfo(m *NodeMetrics) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			m.MemTotalKB = v
		case "MemAvailable:":
			m.MemAvailKB = v
		case "SwapTotal:":
			m.SwapTotalKB = v
		case "SwapFree:":
			m.SwapFreeKB = v
		}
	}
}

// diskStats возвращает [totalKB, usedKB, availKB] через statfs(2).
func diskStats(path string) ([3]uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return [3]uint64{}, err
	}
	bs := uint64(st.Bsize)
	total := st.Blocks * bs / 1024
	avail := st.Bavail * bs / 1024
	used := (st.Blocks - st.Bfree) * bs / 1024
	return [3]uint64{total, used, avail}, nil
}

func readUptime() (uint64, error) {
	s := readTrim("/proc/uptime")
	f := strings.Fields(s)
	if len(f) < 1 {
		return 0, os.ErrNotExist
	}
	v, err := strconv.ParseFloat(f[0], 64)
	return uint64(v), err
}

// readBattery ищет первую батарею в /sys/class/power_supply.
func readBattery() *BatteryInfo {
	entries, err := os.ReadDir("/sys/class/power_supply")
	if err != nil {
		return nil
	}
	for _, e := range entries {
		base := filepath.Join("/sys/class/power_supply", e.Name())
		if readTrim(filepath.Join(base, "type")) != "Battery" {
			continue
		}
		b := &BatteryInfo{Status: "Unknown"}
		if v := readTrim(filepath.Join(base, "status")); v != "" {
			b.Status = v
		}
		if p, err := strconv.Atoi(readTrim(filepath.Join(base, "capacity"))); err == nil {
			b.Percent = p
		}
		// энергия в мкВт·ч → Вт·ч (у кого-то charge_* в мкА·ч — берём как есть, только проценты точны)
		if v, err := strconv.ParseFloat(readTrim(filepath.Join(base, "energy_now")), 64); err == nil {
			b.EnergyNow = v / 1e6
		}
		if v, err := strconv.ParseFloat(readTrim(filepath.Join(base, "energy_full")), 64); err == nil {
			b.EnergyMax = v / 1e6
		}
		if v, err := strconv.ParseFloat(readTrim(filepath.Join(base, "power_now")), 64); err == nil {
			b.RateW = v / 1e6
		}
		return b
	}
	return nil
}

// readThermal собирает зоны thermal_zone* (миллиградусы → °C).
func readThermal() map[string]float64 {
	entries, err := os.ReadDir("/sys/class/thermal")
	if err != nil {
		return nil
	}
	out := make(map[string]float64)
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "thermal_zone") {
			continue
		}
		base := filepath.Join("/sys/class/thermal", e.Name())
		name := readTrim(filepath.Join(base, "type"))
		if name == "" {
			name = e.Name()
		}
		if v, err := strconv.ParseFloat(readTrim(filepath.Join(base, "temp")), 64); err == nil {
			out[name] = v / 1000
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

var _ Collector = (*LocalCollector)(nil)
var _ Collector = (*remoteCollector)(nil)

// Collector — единый интерфейс сборщика метрик ноды.
type Collector interface {
	Collect(ctx context.Context) *NodeMetrics
}

// RemoteConfig — параметры удалённого коллектора (для NewRemoteCollector).
type RemoteConfig struct {
	Timeout  time.Duration
	Services []string
}

// NewRemoteCollector строит удалённый коллектор на базе общего исполнителя.
func NewRemoteCollector(exec Runner, cfg RemoteConfig, log *slog.Logger) Collector {
	return &remoteCollector{exec: exec, cfg: cfg, log: log}
}
