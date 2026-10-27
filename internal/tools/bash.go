package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/archi/archi/internal/bridge"
)

// registerBash — local_bash / remote_bash: deny-паттерны, таймаут, лимит вывода.
func (r *Registry) registerBash() {
	r.Register(&Tool{
		Name: "local_bash",
		Description: "Исполнить shell-команду на НОУТБУКЕ (local). Для диагностики, " +
			"управления файлами и лёгких задач. Вывод ограничен 256 KiB.",
		Timeout: 3 * time.Minute,
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command":     map[string]any{"type": "string", "description": "bash-команда (одна строка)"},
				"timeout_sec": map[string]any{"type": "integer", "description": "максимум 300"},
			},
			"required": []string{"command"},
		},
		Exec: r.execBash(bridge.NodeLocal),
	})
	r.Register(&Tool{
		Name: "remote_bash",
		Description: "Исполнить shell-команду на GCP VM agrobot-prod (remote). БЕРЕГИ РЕСУРСЫ: " +
			"1 vCPU и 1 ГБ RAM, рядом прод-сервисы (PostgreSQL, agrobot, info-bot). " +
			"Запрещено: grep -r по большим деревьям, find / без пути, gzip больших файлов.",
		Timeout: 3 * time.Minute,
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command":     map[string]any{"type": "string", "description": "bash-команда (одна строка)"},
				"timeout_sec": map[string]any{"type": "integer", "description": "максимум 300"},
			},
			"required": []string{"command"},
		},
		Exec: r.execBash(bridge.NodeRemote),
	})
}

func (r *Registry) execBash(node string) func(context.Context, json.RawMessage) (string, bool) {
	return func(ctx context.Context, args json.RawMessage) (string, bool) {
		cmd := argString(args, "command", "")
		if cmd == "" {
			return "ОШИБКА: параметр command обязателен", true
		}
		if bad := r.matchDeny(cmd); bad != "" {
			r.deps.Log.Warn("tools: команда заблокирована deny-паттерном", "node", node, "cmd", cmd, "pattern", bad)
			return fmt.Sprintf("ОШИБКА: команда отклонена санитайзером (паттерн %s). "+
				"Деструктивные операции требуют человека.", bad), true
		}
		timeout := time.Duration(argInt(args, "timeout_sec", 120)) * time.Second
		if timeout <= 0 || timeout > 5*time.Minute {
			timeout = 5 * time.Minute
		}
		res, err := r.deps.Bridge.Executor().Run(ctx, node, cmd, timeout, r.deps.Cfg.Bridge.MaxOutputBytes)
		if err != nil {
			return fmt.Sprintf("ОШИБКА запуска на %s: %v", node, err), true
		}
		return formatExecResult(res), res.ExitCode != 0
	}
}

func (r *Registry) matchDeny(cmd string) string {
	for _, re := range r.deny {
		if re.MatchString(cmd) {
			return re.String()
		}
	}
	return ""
}

// formatExecResult — единый формат вывода bash-инструментов для LLM.
func formatExecResult(res *bridge.ExecResult) string {
	out := fmt.Sprintf("node=%s exit_code=%d duration=%s", res.Node, res.ExitCode, res.Duration.Round(time.Millisecond))
	if res.Truncated {
		out += " [вывод урезан]"
	}
	if s := res.Stdout; s != "" {
		out += "\n--- stdout ---\n" + s
	}
	if s := res.Stderr; s != "" {
		out += "\n--- stderr ---\n" + s
	}
	if res.Stdout == "" && res.Stderr == "" {
		out += "\n(пустой вывод)"
	}
	return out
}
