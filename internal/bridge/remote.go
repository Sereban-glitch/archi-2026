package bridge

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// remoteCollector — собирает метрики agrobot-prod одной SSH-сессией
// (POSIX-sh скрипт печатает key-value, Go-сторона парсит).
// Не плодит десятки соединений: один запрос — одна сессия из пула.
type remoteCollector struct {
	exec Runner
	cfg  RemoteConfig
	log  *slog.Logger
}

// buildProbeScript генерирует POSIX-sh скрипт диагностики VM.
// Имена сервисов вставляются в одинарных кавычках после строгой фильтрации (анти-инъекция).
func buildProbeScript(services []string) string {
	var sb strings.Builder
	sb.WriteString(`
h=$(hostname); echo "host $h"
echo "kernel $(uname -r)"
u=0 n=0 s=0 id=0 io=0 irq=0 sf=0 st=0
read -r _ u n s id io irq sf st _ < /proc/stat 2>/dev/null || true
t1=$((u+n+s+id+io+irq+sf+st)); i1=$id
sleep 1
read -r _ u n s id io irq sf st _ < /proc/stat 2>/dev/null || true
t2=$((u+n+s+id+io+irq+sf+st)); i2=$id
d=$((t2-t1)); di=$((i2-i1))
if [ "$d" -gt 0 ]; then echo "cpu_busy_p $((100*(d-di)/d))"; else echo "cpu_busy_p 0"; fi
echo "load $(cut -d' ' -f1-3 /proc/loadavg)"
echo "mem_total_kb $(awk '/^MemTotal:/{print $2}' /proc/meminfo)"
echo "mem_avail_kb $(awk '/^MemAvailable:/{print $2}' /proc/meminfo)"
echo "swap_total_kb $(awk '/^SwapTotal:/{print $2}' /proc/meminfo)"
echo "swap_free_kb $(awk '/^SwapFree:/{print $2}' /proc/meminfo)"
echo "disk_kb $(df -kP / | awk 'NR==2{print $2, $3, $4}')"
echo "uptime_s $(cut -d. -f1 /proc/uptime)"
`)
	for _, s := range services {
		if !safeServiceName(s) {
			continue
		}
		fmt.Fprintf(&sb, "echo \"svc %s $(systemctl is-active %s 2>/dev/null || echo unknown)\"\n",
			shellQuote(s), shellQuote(s))
	}
	return sb.String()
}

// safeServiceName — сервисы приходят из локального конфига, но всё равно фильтруем жёстко.
func safeServiceName(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '.' || r == '-' || r == '_' || r == '@' || r == ':') {
			return false
		}
	}
	return true
}

func shellQuote(s string) string { return "'" + s + "'" }

// Collect снимает метрики удалённой ноды (Online=false при недоступности).
func (c *remoteCollector) Collect(ctx context.Context) *NodeMetrics {
	m := &NodeMetrics{
		Node:      NodeRemote,
		Timestamp: time.Now().UTC(),
		Online:    false,
	}
	timeout := c.cfg.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	res, err := c.exec.Run(ctx, NodeRemote, buildProbeScript(c.cfg.Services), timeout, 64*1024)
	if err != nil {
		m.Error = err.Error()
		return m
	}
	if res.ExitCode != 0 && strings.TrimSpace(res.Stdout) == "" {
		m.Error = fmt.Sprintf("probe exit=%d stderr=%s", res.ExitCode, strings.TrimSpace(res.Stderr))
		return m
	}
	m.Online = true
	parseProbeOutput(res.Stdout, m)
	if m.Hostname == "" {
		m.Hostname = "unknown"
	}
	return m
}

// parseProbeOutput разбирает строки `key value...` из вывода скрипта.
func parseProbeOutput(out string, m *NodeMetrics) {
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 64*1024), 64*1024)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "host":
			m.Hostname = f[1]
		case "kernel":
			m.Kernel = f[1]
		case "cpu_busy_p":
			m.CPUPercent, _ = strconv.ParseFloat(f[1], 64)
		case "load":
			if len(f) >= 4 {
				m.Load1, _ = strconv.ParseFloat(f[1], 64)
				m.Load5, _ = strconv.ParseFloat(f[2], 64)
				m.Load15, _ = strconv.ParseFloat(f[3], 64)
			}
		case "mem_total_kb":
			m.MemTotalKB, _ = strconv.ParseUint(f[1], 10, 64)
		case "mem_avail_kb":
			m.MemAvailKB, _ = strconv.ParseUint(f[1], 10, 64)
		case "swap_total_kb":
			m.SwapTotalKB, _ = strconv.ParseUint(f[1], 10, 64)
		case "swap_free_kb":
			m.SwapFreeKB, _ = strconv.ParseUint(f[1], 10, 64)
		case "disk_kb":
			if len(f) >= 4 {
				m.DiskTotalKB, _ = strconv.ParseUint(f[1], 10, 64)
				m.DiskUsedKB, _ = strconv.ParseUint(f[2], 10, 64)
				m.DiskAvailKB, _ = strconv.ParseUint(f[3], 10, 64)
			}
		case "uptime_s":
			m.UptimeSec, _ = strconv.ParseUint(f[1], 10, 64)
		case "svc":
			if len(f) >= 3 {
				m.Services = append(m.Services, ServiceState{Name: f[1], State: f[2]})
			}
		}
	}
}
