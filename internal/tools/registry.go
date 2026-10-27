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
        mu    sync.RWMutex
        tools map[string]*Tool
        order []string
        deny  []*regexp.Regexp
        deps  Deps
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
        r.registerBash()
        r.registerFS()
        r.registerSystemd()
        r.registerTelemetry()
        r.registerKnowledge()
        return r
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
                        text = text[:r.deps.Cfg.Tools.MaxReadBytes] + "\n…[вывод урезан реестром]"
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
