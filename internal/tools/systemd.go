package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/archi/archi/internal/bridge"
)

// registerSystemd — статус/рестарт/логи systemd на обеих нодах.
func (r *Registry) registerSystemd() {
	r.Register(&Tool{
		Name: "systemd_status",
		Description: "Статус systemd-юнитов на любой ноде: state, главный PID, память (MemoryCurrent), " +
			"последние юнит-ошибки. Для deep-диагностики journalctl смотри bash-инструменты.",
		Timeout: 30 * time.Second,
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"node":    nodeEnum(),
				"service": map[string]any{"type": "string", "description": "имя юнита, напр. console-bot.service"},
			},
			"required": []string{"node", "service"},
		},
		Exec: r.execSystemdStatus,
	})
	r.Register(&Tool{
		Name: "systemd_restart",
		Description: "Перезапустить systemd-сервис. РАЗРЕШЕНО только для: " +
			strings.Join([]string{"console-bot", "agrobot", "info-bot", "archi"}, ", ") +
			". На remote требует passwordless sudo (sudo -n). Используй осознанно.",
		Timeout: 60 * time.Second,
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"node":    nodeEnum(),
				"service": map[string]any{"type": "string"},
			},
			"required": []string{"node", "service"},
		},
		Exec: r.execSystemdRestart,
	})
}

// normalizeUnit — приводим имя к *.service, если суффикса нет.
func normalizeUnit(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, ".") {
		s += ".service"
	}
	return s
}

func safeUnit(s string) bool {
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '.' || r == '-' || r == '_' || r == '@' || r == ':') {
			return false
		}
	}
	return !strings.ContainsAny(s, "/\\ '\"\t\n$;&|`(){}")
}

func (r *Registry) execSystemdStatus(ctx context.Context, args json.RawMessage) (string, bool) {
	node := argString(args, "node", bridge.NodeLocal)
	unit := normalizeUnit(argString(args, "service", ""))
	if unit == "" {
		return "ОШИБКА: service обязателен", true
	}
	if !safeUnit(unit) {
		return "ОШИБКА: недопустимое имя юнита", true
	}
	cmd := fmt.Sprintf(
		`systemctl show %s --property=ActiveState,SubState,MainPID,MemoryCurrent,ExecMainStartTimestamp,NRestarts --no-pager`, shellQ(unit))
	res, err := r.deps.Bridge.Executor().Run(ctx, node, cmd, 30*time.Second, 32*1024)
	if err != nil {
		return fmt.Sprintf("ОШИБКА SSH: %v", err), true
	}
	out := fmt.Sprintf("=== systemd %s @ %s ===\n", unit, node)
	out += res.Stdout
	if s := strings.TrimSpace(res.Stderr); s != "" {
		out += "stderr: " + s + "\n"
	}
	if strings.Contains(res.Stdout, "ActiveState=inactive") || strings.Contains(res.Stdout, "ActiveState=failed") {
		return out, true
	}
	return out, false
}

func (r *Registry) execSystemdRestart(ctx context.Context, args json.RawMessage) (string, bool) {
	node := argString(args, "node", bridge.NodeLocal)
	unit := normalizeUnit(argString(args, "service", ""))
	if unit == "" {
		return "ОШИБКА: service обязателен", true
	}
	if !safeUnit(unit) {
		return "ОШИБКА: недопустимое имя юнита", true
	}
	if !r.restartAllowed(unit) {
		return fmt.Sprintf("ОШИБКА: сервис %s не входит в tools.allowed_restart_services — рестарт запрещён политикой", unit), true
	}
	prefix := ""
	if node == bridge.NodeRemote {
		prefix = "sudo -n "
	}
	cmd := prefix + "systemctl restart " + shellQ(unit) + " && sleep 2 && systemctl is-active " + shellQ(unit)
	res, err := r.deps.Bridge.Executor().Run(ctx, node, cmd, 60*time.Second, 16*1024)
	if err != nil {
		return fmt.Sprintf("ОШИБКА SSH: %v", err), true
	}
	if res.ExitCode != 0 {
		return fmt.Sprintf("РЕСТАРТ НЕ УДАЛСЯ:\n%s", formatExecResult(res)), true
	}
	r.deps.Log.Warn("tools: выполнен systemd_restart", "node", node, "unit", unit)
	_ = r.deps.Memory.Save("episode", fmt.Sprintf("Перезапущен сервис %s на ноде %s командой агента", unit, node), "systemd", node)
	return fmt.Sprintf("OK: %s перезапущен на %s, состояние: %s", unit, node, strings.TrimSpace(res.Stdout)), false
}

func (r *Registry) restartAllowed(unit string) bool {
	base := strings.TrimSuffix(unit, ".service")
	for _, a := range r.deps.Cfg.Tools.AllowedRestart {
		if a == unit || a == base {
			return true
		}
	}
	return false
}

// registerTelemetry — node_status: свежий снимок ноды из моста (или цикл сбора по требованию).
func (r *Registry) registerTelemetry() {
	r.Register(&Tool{
		Name: "node_status",
		Description: "Актуальные метрики ноды: CPU, RAM, диск, load, uptime, сервисы, батарея (для ноутбука). " +
			"Сначала проверь готовый снимок, телеметрия обновляется каждые ~30 c.",
		Timeout: 45 * time.Second,
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"node": nodeEnum(),
			},
			"required": []string{"node"},
		},
		Exec: r.execNodeStatus,
	})
}

func (r *Registry) execNodeStatus(ctx context.Context, args json.RawMessage) (string, bool) {
	node := argString(args, "node", bridge.NodeLocal)
	snap := r.deps.Bridge.Snapshot()
	var m *bridge.NodeMetrics
	if snap != nil {
		switch node {
		case bridge.NodeLocal:
			m = snap.Local
		case bridge.NodeRemote:
			m = snap.Remote
		}
	}
	if m == nil || time.Since(m.Timestamp) > 90*time.Second {
		// снимок устарел — собираем напрямую
		cmd := "uptime && free -m && df -kP / | tail -1 && hostname"
		res, err := r.deps.Bridge.Executor().Run(ctx, node, cmd, 30*time.Second, 16*1024)
		if err != nil {
			return fmt.Sprintf("ОШИБКА сбора метрик %s: %v", node, err), true
		}
		return fmt.Sprintf("=== %s (свежий сбор) ===\n%s", node, res.Stdout), res.ExitCode != 0
	}
	return formatMetrics(m), false
}

func formatMetrics(m *bridge.NodeMetrics) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "=== %s (%s) ===\n", m.Node, m.Hostname)
	if !m.Online {
		fmt.Fprintf(&sb, "🔴 НЕДОСТУПЕН: %s\n", m.Error)
		return sb.String()
	}
	fmt.Fprintf(&sb, "CPU: %.0f%% | load: %.2f %.2f %.2f\n", m.CPUPercent, m.Load1, m.Load5, m.Load15)
	fmt.Fprintf(&sb, "RAM: %.1f%% (доступно %s / %s)\n", m.MemPercent(), kbH(m.MemAvailKB), kbH(m.MemTotalKB))
	if m.SwapTotalKB > 0 {
		fmt.Fprintf(&sb, "Swap: занято %s из %s\n", kbH(m.SwapTotalKB-m.SwapFreeKB), kbH(m.SwapTotalKB))
	}
	fmt.Fprintf(&sb, "Диск /: %.1f%% (свободно %s)\n", m.DiskPercent(), kbH(m.DiskAvailKB))
	fmt.Fprintf(&sb, "Uptime: %s | ядро %s\n", humanDur(m.UptimeSec), m.Kernel)
	if m.Battery != nil {
		fmt.Fprintf(&sb, "Батарея: %d%% (%s", m.Battery.Percent, m.Battery.Status)
		if m.Battery.RateW > 0 {
			fmt.Fprintf(&sb, ", %.1f Вт", m.Battery.RateW)
		}
		sb.WriteString(")\n")
	}
	if len(m.Temperatures) > 0 {
		keys := make([]string, 0, len(m.Temperatures))
		for k := range m.Temperatures {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		sb.WriteString("Температуры: ")
		for i, k := range keys {
			if i > 0 {
				sb.WriteString(", ")
			}
			fmt.Fprintf(&sb, "%s=%.0f°C", k, m.Temperatures[k])
		}
		sb.WriteString("\n")
	}
	for _, s := range m.Services {
		fmt.Fprintf(&sb, "  %s: %s\n", s.Name, s.State)
	}
	return sb.String()
}

func kbH(kb uint64) string {
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

func humanDur(sec uint64) string {
	d := time.Duration(sec) * time.Second
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm%ds", int(d.Minutes())%60, int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}
