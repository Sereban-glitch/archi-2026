// Package metrics — лёгкий счётчикный реестр в формате Prometheus text
// (GAP-аудит 2026: observability). Никаких тяжёлых зависимостей: один мьютекс,
// рендер sorted, HTTP-эндпоинт только на localhost.
//
//	archi_llm_requests_total 42
//	archi_tool_calls_total{tool="bash",result="ok"} 17
//	archi_uptime_seconds 3600
package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type counter struct {
	labels string // {k="v",...} в порядке сортировки
	value  float64
}

type gauge struct {
	name string
	fn   func() float64
}

var (
	mu       sync.RWMutex
	counters = make(map[string]*counter)
	gauges   = make([]gauge, 0, 8)
	order    []string // ключи counters в порядке создания — стабильный вывод
	start    = time.Now()
)

// labelKey — стабильный ключ счётчика: имя + отсортированные пары label=value.
func labelKey(name string, labelValues []string) string {
	if len(labelValues)%2 != 0 {
		labelValues = append(append([]string(nil), labelValues[:len(labelValues)-1]...), "unpaired")
	}
	var pairs []string
	for i := 0; i+1 < len(labelValues); i += 2 {
		pairs = append(pairs, labelValues[i]+"="+labelValues[i+1])
	}
	sort.Strings(pairs)
	return name + "|" + strings.Join(pairs, ",")
}

// renderLabels — {k="v",...} для Prometheus.
func renderLabels(labelValues []string) string {
	if len(labelValues) < 2 {
		return ""
	}
	var pairs []string
	for i := 0; i+1 < len(labelValues); i += 2 {
		pairs = append(pairs, fmt.Sprintf("%s=%q", labelValues[i], labelValues[i+1]))
	}
	sort.Strings(pairs)
	return "{" + strings.Join(pairs, ",") + "}"
}

// Inc инкрементирует счётчик. labelValues — пары "ключ", "значение": ("tool", "bash", "result", "ok").
func Inc(name string, labelValues ...string) {
	Add(name, 1, labelValues...)
}

// Add прибавляет к счётчику произвольную величину (токены, секунды).
func Add(name string, v float64, labelValues ...string) {
	key := labelKey(name, labelValues)
	mu.Lock()
	defer mu.Unlock()
	c, ok := counters[key]
	if !ok {
		c = &counter{labels: renderLabels(labelValues)}
		counters[key] = c
		order = append(order, key)
	}
	c.value += v
}

// RegisterGauge регистрирует вычисляемую метрику (вызывается при рендере).
func RegisterGauge(name string, fn func() float64) {
	mu.Lock()
	defer mu.Unlock()
	for _, g := range gauges {
		if g.name == name {
			return
		}
	}
	gauges = append(gauges, gauge{name: name, fn: fn})
}

// Render отдаёт весь реестр в text exposition format.
func Render() string {
	mu.RLock()
	defer mu.RUnlock()
	var sb strings.Builder
	for _, key := range order {
		c := counters[key]
		fmt.Fprintf(&sb, "%s%s %g\n", key[:strings.IndexByte(key, '|')], c.labels, c.value)
	}
	for _, g := range gauges {
		fmt.Fprintf(&sb, "%s %g\n", g.name, g.fn())
	}
	return sb.String()
}

// Reset очищает реестр (только для тестов).
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	counters = make(map[string]*counter)
	gauges = gauges[:0]
	order = order[:0]
}

// Serve поднимает HTTP: /metrics (Prometheus) и /healthz. Блокирует до отмены ctx.
// addr — только 127.0.0.1:port по умолчанию; не вешай на 0.0.0.0 без нужды.
func Serve(ctx context.Context, addr, version string, log *slog.Logger) error {
	if addr == "" {
		addr = "127.0.0.1:9109"
	}
	RegisterGauge("archi_uptime_seconds", func() float64 { return time.Since(start).Seconds() })

	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(Render()))
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("metrics: listen %s: %w", addr, err)
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	log.Info("metrics: Prometheus-эндпоинт поднят", "addr", addr, "path", "/metrics")
	if err := srv.Serve(ln); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

// Hostname для лейблов — ошибку глотаем: метрики не должны падать.
func Hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}
