// approval.go — интерактивное подтверждение опасных команд владельцем.
//
// Паттерн 2026 (OpenAI Operator / Claude Code / Hermes approval.py): агент
// НЕ исполняет side-effect молча — опасное действие сначала показывается
// человеку (у нас — Telegram inline-клавиатура «Разрешить/Отклонить»),
// и только после явного «Разрешить» команда уходит в executor.
// Нет ответа за approval_timeout — безопасный отказ (fail-closed).
package tools

import (
	"context"
	"fmt"
)

// confirm — точка принятия решения: опасная команда узла node совпала с паттерном pat.
func (r *Registry) confirm(ctx context.Context, node, cmd, pat string) (bool, error) {
	r.mu.RLock()
	approver := r.approver
	timeout := r.deps.Cfg.Tools.ApprovalTimeout.Duration
	r.mu.RUnlock()

	summary := fmt.Sprintf("🔐 Archi просит подтверждение\n\nнода: %s\nкоманда: %s\nпаттерн: %s", node, clipCmd(cmd), pat)

	if approver == nil {
		// Канала подтверждения нет (Telegram выключен / запрос пришёл по IPC
		// раньше, чем поднялся бот) — fail-closed: отказ без исполнения.
		r.deps.Log.Warn("tools: approval нужен, но approver не подключён — отказ (fail-closed)",
			"node", node, "cmd", cmd, "pattern", pat)
		return false, fmt.Errorf("некого спросить: Telegram-канал подтверждений недоступен, политика безопасности запрещает исполнение без подтверждения")
	}

	if timeout <= 0 {
		timeout = 120 * 1000_000_000 // 2 минуты (нано)
	}
	actx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	r.deps.Log.Warn("tools: запрос подтверждения у владельца", "node", node, "cmd", cmd, "pattern", pat, "timeout", timeout.String())
	ok, err := approver.Ask(actx, summary)
	if err != nil {
		return false, fmt.Errorf("канал подтверждений: %w", err)
	}
	if ok {
		r.deps.Log.Warn("tools: владелец ОДОБРИЛ команду", "node", node, "cmd", cmd)
		_ = r.deps.Memory.Save("episode", "Владелец одобрил команду агента: "+cmd, "approval", node)
	} else {
		r.deps.Log.Warn("tools: владелец отклонил команду", "node", node, "cmd", cmd)
	}
	return ok, nil
}

// clipCmd — компактная команда для сводки (полная — в логах).
func clipCmd(c string) string {
	if len(c) > 300 {
		return c[:300] + "…"
	}
	return c
}
