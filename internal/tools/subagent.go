// subagent.go — делегирование подзадач субагенту с чистым контекстом
// (паттерн Claude Code sub-agents / Hermes delegate_task). Субагент получает
// ту же панель инструментов, но СВОЮ сессию: контекст родителя не раздувается,
// а длинные разведки не вытесняют основной диалог.
//
// Защита от рекурсии: глобальный счётчик глубины — субагент не может
// запустить своего субагента (иначе экспоненциальный веер LLM-вызовов.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"
)

// SubAgentRunner — контракт для инъекции агента (реализуется *agent.Agent).
// Создаётся после реестра (агенту нужен сам реестр) — см. SetSubAgent.
type SubAgentRunner interface {
	Handle(ctx context.Context, sessionID, userText string) (string, error)
}

// delegateDepth — 0 = мы в основном агенте, 1 = внутри субагента.
var delegateDepth atomic.Int32

// SetSubAgent подключает делегирование (после создания агента в main).
// ВАЖНО: Register берёт r.mu сам — не держать лок при вызове (иначе дедлок).
func (r *Registry) SetSubAgent(runner SubAgentRunner) {
	r.mu.Lock()
	r.subAgent = runner
	r.mu.Unlock()
	if runner != nil {
		// регистрируем только при живом субагенте — LLM не должен видеть мёртвый инструмент
		r.registerSubagent()
	}
}

func (r *Registry) registerSubagent() {
	r.Register(&Tool{
		Name: "delegate_task",
		Description: "Делегировать подзадачу субагенту с чистым контекстом. " +
			"Используй для длинных разведок (поиск по логам, анализ больших файлов, перебор вариантов): " +
			"субагент отработает самостоятельно и вернёт готовый вывод, не засоряя наш диалог. " +
			"Субагент НЕ видит нашу переписку — формулируй задачу самодостаточно.",
		Timeout: 6 * time.Minute,
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task": map[string]any{"type": "string",
					"description": "самодостаточная формулировка подзадачи: контекст, что найти/сделать, формат ответа"},
			},
			"required": []string{"task"},
		},
		Exec: r.execDelegate,
	})
}

func (r *Registry) execDelegate(ctx context.Context, args json.RawMessage) (string, bool) {
	r.mu.RLock()
	runner := r.subAgent
	r.mu.RUnlock()

	task := argString(args, "task", "")
	if task == "" {
		return "ОШИБКА: task обязателен", true
	}
	if runner == nil {
		return "ОШИБКА: делегирование не настроено (субагент не подключён)", true
	}
	// Глубина 1: субагент внутри субагента запрещён — иначе веер вызовов.
	if !delegateDepth.CompareAndSwap(0, 1) {
		return "ОШИБКА: субагент уже запущен — вложенное делегирование запрещено. Доработай задачу сам.", true
	}
	defer delegateDepth.Store(0)

	// Отдельный таймаут на подзадачу: не больше tool-таймаута, но и не навсегда.
	dctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	session := fmt.Sprintf("sub:%d", time.Now().UnixNano())
	reply, err := runner.Handle(dctx, session, task)
	if err != nil {
		return fmt.Sprintf("Субагент не справился: %v", err), true
	}
	if len(reply) > 8000 {
		reply = reply[:8000] + "\n…[вывод субагента урезан]"
	}
	if reply == "" {
		return "Субагент вернул пустой ответ", true
	}
	return "Ответ субагента:\n" + reply, false
}
