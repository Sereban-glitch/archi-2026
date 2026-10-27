// Package tools — реестр инструментов Archi: JSON Schema определения для LLM
// и безопасное исполнение с таймаутами, паник-гардами и лимитами вывода.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/archi/archi/internal/agent"
	"github.com/archi/archi/internal/bridge"
	"github.com/archi/archi/internal/config"
)

// Deps — зависимости, внедряемые в инструменты.
type Deps struct {
	Cfg    config.Config
	Bridge *bridge.Bridge
	Memory *agent.Memory
	Log    *slog.Logger
}

// Approver — канал подтверждения опасных действий у владельца.
// Реализация — Telegram inline-клавиатура (telegram.Runner.Ask);
// nil → политика безопасности падает в отказ (см. tools/approval.go).
type Approver interface {
	Ask(ctx context.Context, summary string) (approved bool, err error)
}

// Tool — единица способности агента.
type Tool struct {
	Name        string
	Description string
	Schema      map[string]any
	Timeout     time.Duration
	// Exec возвращает текстовый результат для LLM; second return = is_error.
	Exec func(ctx context.Context, args json.RawMessage) (string, bool)
}

// Registry — потокобезопасный реестр инструментов.
type Registry struct {
	mu        sync.RWMutex
	tools     map[string]*Tool
	order     []string
	deny      []*regexp.Regexp // жёсткий запрет — не исполняется никогда
	approval  []*regexp.Regexp // опасное — только после подтверждения владельца
	forbidden []*regexp.Regexp // секретные пути — не читаются
	approver  Approver
	deps      Deps
}

// NewRegistry регистрирует весь стандартный набор Archi.
func NewRegistry(deps Deps) *Registry {
	r := &Registry{
		tools: make(map[string]*Tool),
		deps:  deps,
	}
	for _, p := range deps.Cfg.Tools.BashDenyPatterns {
		if re, err := regexp.Compile(p); err == nil {
			r.deny = append(r.deny, re)
		} else {
			deps.Log.Warn("tools: битый bash_deny_pattern пропущен", "pattern", p, "err", err)
		}
	}
	for _, p := range deps.Cfg.Tools.ApprovalPatterns {
		if re, err := regexp.Compile(p); err == nil {
			r.approval = append(r.approval, re)
		} else {
			deps.Log.Warn("tools: битый approval_pattern пропущен", "pattern", p, "err", err)
		}
	}
	for _, p := range deps.Cfg.Tools.ForbiddenReadPatterns {
		if re, err := regexp.Compile(p); err == nil {
			r.forbidden = append(r.forbidden, re)
		} else {
			deps.Log.Warn("tools: битый forbidden_read_pattern", "pattern", p, "err", err)
		}
	}
	r.registerBash()
	r.registerFS()
	r.registerSystemd()
	r.registerTelemetry()
	r.registerKnowledge()
	return r
}

// SetApprover подключает канал подтверждений (вызывается после старта Telegram).
func (r *Registry) SetApprover(a Approver) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.approver = a
}

// Register добавляет инструмент (для расширений).
func (r *Registry) Register(t *Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.tools[t.Name]; dup {
		r.deps.Log.Error("tools: дубликат имени инструмента", "name", t.Name)
		return
	}
	r.tools[t.Name] = t
	r.order = append(r.order, t.Name)
}

// Definitions отдаёт список для API Anthropic (tools: [{name, description, input_schema}]).
func (r *Registry) Definitions() []map[string]any {
	r.mu.RLock()
	defer r.mu.RUnlock()
	defs := make([]map[string]any, 0, len(r.order))
	for _, name := range r.order {
		t := r.tools[name]
		defs = append(defs, map[string]any{
			"name":         t.Name,
			"description":  t.Description,
			"input_schema": t.Schema,
		})
	}
	return defs
}

// Call исполняет инструмент по имени. Паники ловятся, таймаут принудителен,
// вывод урезается — LLM никогда не должен видеть сырую бесконечность.
func (r *Registry) Call(ctx context.Context, name string, input json.RawMessage) (string, bool) {
	r.mu.RLock()
	t, ok := r.tools[name]
	r.mu.RUnlock()
	if !ok {
		return fmt.Sprintf("ОШИБКА: неизвестный инструмент %q (доступны: %v)", name, r.order), true
	}
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = r.deps.Cfg.Bridge.CommandTimeout.Duration
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type outcome struct {
		text  string
		isErr bool
	}
	ch := make(chan outcome, 1)
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				r.deps.Log.Error("tools: паника инструмента", "tool", name, "panic", rec)
				ch <- outcome{fmt.Sprintf("ПАНИКА инструмента %s: %v (восстановлено)", name, rec), true}
			}
		}()
		text, isErr := t.Exec(cctx, input)
		if len(text) > r.deps.Cfg.Tools.MaxReadBytes {
			text = truncateUTF8(text, r.deps.Cfg.Tools.MaxReadBytes)
		}
		ch <- outcome{text, isErr}
	}()
	select {
	case o := <-ch:
		return o.text, o.isErr
	case <-cctx.Done():
		return fmt.Sprintf("ОШИБКА: инструмент %s прерван: %v", name, cctx.Err()), true
	}
}

// truncateUTF8 — усечение результата по байтам без разрыва UTF-8 рун
// (битая руна в tool_result ломала парсинг на стороне API, аудит 2026-10).
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// прямой скан: end всегда стоит на границе руны
	end := 0
	for end < n {
		_, size := utf8.DecodeRuneInString(s[end:])
		if end+size > n {
			break // разрез попал внутрь руны — не берём её
		}
		end += size
	}
	return s[:end] + "\n…[вывод урезан реестром]"
}

// node описывает общий параметр "node" для схем.
func nodeEnum() map[string]any {
	return map[string]any{"type": "string", "enum": []string{bridge.NodeLocal, bridge.NodeRemote},
		"description": "целевая машина: local (ноутбук) или remote (agrobot-prod)"}
}

// argString/argInt — компактные хелперы разбора JSON-аргументов с дефолтами.
func argString(args json.RawMessage, key, def string) string {
	if len(args) == 0 {
		return def
	}
	var m map[string]any
	if err := json.Unmarshal(args, &m); err != nil {
		return def
	}
	if v, ok := m[key].(string); ok && v != "" {
		return v
	}
	return def
}

func argInt(args json.RawMessage, key string, def int) int {
	if len(args) == 0 {
		return def
	}
	var m map[string]any
	if err := json.Unmarshal(args, &m); err != nil {
		return def
	}
	if v, ok := m[key].(float64); ok {
		return int(v)
	}
	return def
}
