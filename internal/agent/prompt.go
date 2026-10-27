package agent

import (
	"fmt"
	"strings"
	"time"

	"github.com/archi/archi/internal/bridge"
	"github.com/archi/archi/internal/skills"
)

// BuildSystemPrompt собирает динамический системный промпт:
// живое состояние обеих машин + идентичность + политика инструментов +
// релевантная память и навыки. Паттерн 2026: агент «видит» телеметрию в каждом
// шаге — без слепых догадок.
func BuildSystemPrompt(tz *time.Location, st *bridge.Status, memEntries []Entry, sklDocs []skills.Doc) string {
	var sb strings.Builder
	now := time.Now().In(tz)

	sb.WriteString(`Ты — Archi, автономный DevOps-агент (2026 Edition) и личный наставник создателя по Linux и серверам. Ты управляешь ДВУМЯ машинами:
- local — ноутбук HP ProBook (Linux Mint, 12 ГБ RAM), на нём ты живёшь;
- remote — продакшен GCP VM agrobot-prod (Debian, 1 ГБ RAM, всего 1 vCPU — ОЧЕНЬ береги ресурсы: никаких тяжёлых команд, grep по гигабайтам, gzip -9 и т.п.).

РОЛЬ И ХАРАКТЕР (из soul.md):
- Личный наставник: создатель осваивает инфраструктуру — объясняй сложные серверные вещи простым и понятным языком, спокойно и по шагам.
- Без роботизированных клише («Я всего лишь ИИ...»). Ты опытный старший напарник-исследователь с инженерным складом ума.
- Общайся исключительно на понятном русском языке.

ПРАВИЛА:
1. Отвечай по-русски, кратко и по делу. Технические выводы — с фактами (числа, exit-коды, пути).
2. Инструментам передавай node: "local" или "remote". Не путай ноды.
3. Сначала диагностируй (node_status, systemd_status, чтение логов через bash), потом действуй.
4. Деструктивные операции (systemd_restart, перезапись конфигов, kill) — только с явной просьбы пользователя или при горящем прод-инциденте; объясняй, что сделал.
5. На remote помни про лимит RAM: там PostgreSQL, agrobot, info-bot и antigravity-прокси. MemoryMax=240M у console-bot — это нормально.
6. Если команда не нужна — не выполняй её. Лучше один точный вызов, чем пять слепых.
7. Завершай ответ обычным текстом (без tool_use), когда задача решена или нужен вопрос пользователю.
8. Решил нетривиальную задачу — сохрани процедуру навыком (skill_save), чтобы в следующий раз не думать заново.

`)
	sb.WriteString("Текущее время: " + now.Format("2006-01-02 15:04:05 MST (Mon)") + "\n\n")

	if st != nil {
		sb.WriteString("=== ЖИВАЯ ТЕЛЕМЕТРИЯ ===\n")
		writeNodeMetrics(&sb, "LOCAL (ноутбук)", st.Local)
		writeNodeMetrics(&sb, "REMOTE (agrobot-prod, GCP VM)", st.Remote)
		sb.WriteString("\n")
	} else {
		sb.WriteString("Телеметрия пока не собрана (мост стартует). Пользуйся инструментом node_status.\n\n")
	}

	if len(memEntries) > 0 {
		sb.WriteString("=== РЕЛЕВАНТНАЯ ПАМЯТЬ (долговременная) ===\n")
		for _, e := range memEntries {
			text := e.Text
			if len(text) > 500 {
				text = text[:500] + "…"
			}
			fmt.Fprintf(&sb, "- [%s] %s\n", e.Kind, strings.ReplaceAll(text, "\n", " "))
		}
		sb.WriteString("\n")
	}

	if len(sklDocs) > 0 {
		sb.WriteString("=== РЕЛЕВАНТНЫЕ НАВЫКИ (проверенные процедуры) ===\n")
		for _, d := range sklDocs {
			body := d.Body
			if len(body) > 700 {
				body = body[:700] + "…"
			}
			fmt.Fprintf(&sb, "--- навык: %s (%s)\n%s\n\n", d.Title, d.Name, strings.TrimSpace(body))
		}
	}
	return sb.String()
}

func writeNodeMetrics(sb *strings.Builder, title string, m *bridge.NodeMetrics) {
	if m == nil {
		return
	}
	if !m.Online {
		fmt.Fprintf(sb, "%s: 🔴 НЕДОСТУПЕН (%s)\n", title, m.Error)
		return
	}
	batt := "—"
	if m.Battery != nil {
		batt = fmt.Sprintf("%d%% %s", m.Battery.Percent, m.Battery.Status)
	}
	fmt.Fprintf(sb, "%s: %s | CPU %.0f%% | RAM %.1f%% (свободно %s из %s) | диск %.0f%% (свободно %s) | load %.2f | uptime %s | батарея %s\n",
		title, m.Hostname, m.CPUPercent,
		m.MemPercent(), kbH(m.MemAvailKB), kbH(m.MemTotalKB),
		m.DiskPercent(), kbH(m.DiskAvailKB),
		m.Load1, humanDur(m.UptimeSec), batt)
	for _, s := range m.Services {
		mark := "✅"
		switch s.State {
		case "failed", "inactive":
			mark = "🔴"
		case "activating", "deactivating":
			mark = "🟡"
		}
		fmt.Fprintf(sb, "  %s %s: %s\n", mark, s.Name, s.State)
	}
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
