// Package agent — ReAct-ядро Archi 2026: цикл мышления, вызов инструментов,
// управление сессиями, компактизация контекста и долговременная память.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/archi/archi/internal/config"
)

// ContentBlock — универсальный блок контента Anthropic Messages API
// (text / tool_use / tool_result в одной структуре, marshal корректен для обоих ролей).
type ContentBlock struct {
	Type string `json:"type"` // "text" | "tool_use" | "tool_result"

	// text
	Text string `json:"text,omitempty"`

	// tool_use (assistant)
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result (user)
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

// Message — сообщение диалога (role: user/assistant, content — массив блоков).
type Message struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
}

// Usage — статистика токенов ответа.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Response — ответ модели.
type Response struct {
	ID         string         `json:"id"`
	Model      string         `json:"model"`
	Content    []ContentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      Usage          `json:"usage"`
}

// apiError — тело ошибки Anthropic-совместимого API.
type apiError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Status  int    // HTTP-статус ответа (429/5xx — ретраебельны)
}

type apiErrorEnvelope struct {
	Error apiError `json:"error"`
}

// Request — тело запроса /v1/messages.
type Request struct {
	Model     string           `json:"model"`
	MaxTokens int              `json:"max_tokens"`
	System    string           `json:"system,omitempty"`
	Messages  []Message        `json:"messages"`
	Tools     []map[string]any `json:"tools,omitempty"`
}

// Client — HTTP-клиент к antigravity-claude-proxy (Anthropic Messages API).
// Отказоустойчивость: экспоненциальный backoff + джиттер на 429/5xx/сетевых
// ошибках (включая ECONNRESET — бич node-телеграм-ботов и прокси), honor Retry-After.
// Failover (self-healing, 2027.1): когда основной провайдер исчерпал ретраи,
// запрос уходит по цепочке llm.providers — агент не ложится вместе с одним прокси.
type Client struct {
	primary                 providerTarget
	fallbacks               []providerTarget
	maxTokens               int
	maxRetries              int
	backoffBase, backoffMax time.Duration
	http                    *http.Client
	log                     *slog.Logger
}

// providerTarget — один Anthropic-совместимый endpoint цепочки.
type providerTarget struct {
	base  string
	key   string
	model string
}

// NewClient создаёт LLM-клиент с опциональной failover-цепочкой.
func NewClient(cfg config.LLMConfig, log *slog.Logger) *Client {
	tr := &http.Transport{
		MaxIdleConns:        4,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
		DialContext:         (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	c := &Client{
		primary: providerTarget{
			base:  strings.TrimRight(cfg.BaseURL, "/"),
			key:   cfg.APIKey,
			model: cfg.Model,
		},
		maxTokens:   cfg.MaxTokens,
		maxRetries:  cfg.MaxRetries,
		backoffBase: 1 * time.Second,
		backoffMax:  30 * time.Second,
		http:        &http.Client{Transport: tr},
		log:         log,
	}
	for _, p := range cfg.Providers {
		if p.BaseURL == "" || p.Model == "" {
			continue // валидатор уже ругнулся; здесь просто не падаем
		}
		key := p.APIKey
		if key == "" {
			key = cfg.APIKey // наследуем ключ основного, если не задан
		}
		c.fallbacks = append(c.fallbacks, providerTarget{
			base:  strings.TrimRight(p.BaseURL, "/"),
			key:   key,
			model: p.Model,
		})
	}
	return c
}

// Model возвращает имя основной модели.
func (c *Client) Model() string { return c.primary.model }

// Providers возвращает размер failover-цепочки (0 = только основной).
func (c *Client) Providers() int { return 1 + len(c.fallbacks) }

// SendMessage выполняет запрос к модели с ретраями и failover-цепочкой.
// Системный таймаут — через ctx.
func (c *Client) SendMessage(ctx context.Context, system string, messages []Message, tools []map[string]any) (*Response, error) {
	targets := append([]providerTarget{c.primary}, c.fallbacks...)
	var errs []error
	for ti, tgt := range targets {
		resp, err := c.sendTo(ctx, tgt, system, messages, tools)
		if err == nil {
			if ti > 0 {
				c.log.Warn("llm: ответил резервный провайдер (failover)", "provider", tgt.base, "model", tgt.model)
			}
			return resp, nil
		}
		errs = append(errs, fmt.Errorf("провайдер %d (%s): %w", ti+1, tgt.base, err))
		if ctx.Err() != nil {
			break // отменять цепочку по ctx — правильно (крони/таймауты важнее)
		}
		if len(targets) > 1 {
			c.log.Warn("llm: провайдер не ответил, пробую следующий", "provider", tgt.base, "err", err.Error())
		}
	}
	return nil, fmt.Errorf("llm: все провайдеры недоступны (%d): %w", len(errs), errors.Join(errs...))
}

// sendTo — запросы к одному провайдеру с ретраями.
func (c *Client) sendTo(ctx context.Context, tgt providerTarget, system string, messages []Message, tools []map[string]any) (*Response, error) {
	reqBody, err := json.Marshal(Request{
		Model:     tgt.model,
		MaxTokens: c.maxTokens,
		System:    system,
		Messages:  messages,
		Tools:     tools,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(c.backoff(attempt)):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		resp, apiErr, retryAfter, err := c.post(ctx, tgt, reqBody)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !retryable(apiErr, err) {
			return nil, err
		}
		if retryAfter > 0 {
			select {
			case <-time.After(retryAfter):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		c.log.Warn("llm: повтор запроса", "provider", tgt.base, "attempt", attempt+1, "retry_after", retryAfter.String(), "err", err.Error())
	}
	return nil, fmt.Errorf("llm: исчерпаны попытки (%d): %w", c.maxRetries+1, lastErr)
}

// post — одна HTTP-попытка к провайдеру tgt. Возвращает (ответ, apiErr, retryAfter, error).
func (c *Client) post(ctx context.Context, tgt providerTarget, body []byte) (*Response, *apiError, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tgt.base+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", tgt.key)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.http.Do(req)
	if err != nil {
		// Разбираём сетевые ошибки: ECONNRESET/ECONNREFUSED/EOF/timeout — ретраебельно.
		if isRetryableNet(err) {
			return nil, nil, 0, fmt.Errorf("сеть: %w", err)
		}
		return nil, nil, 0, fmt.Errorf("запрос: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, nil, 0, fmt.Errorf("читаю тело: %w", err)
	}

	if resp.StatusCode >= 400 {
		var env apiErrorEnvelope
		_ = json.Unmarshal(raw, &env)
		ae := env.Error
		if ae.Message == "" {
			ae = apiError{Type: "http_" + resp.Status, Message: strings.TrimSpace(string(raw))}
			if len(ae.Message) > 400 {
				ae.Message = ae.Message[:400]
			}
		}
		ae.Status = resp.StatusCode
		var retryAfter time.Duration
		if resp.StatusCode == 429 {
			if v := resp.Header.Get("Retry-After"); v != "" {
				if sec, err := strconv.Atoi(v); err == nil {
					retryAfter = time.Duration(sec) * time.Second
				}
			}
		}
		return nil, &ae, retryAfter, fmt.Errorf("API %d: %s: %s", resp.StatusCode, ae.Type, ae.Message)
	}

	var out Response
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, nil, 0, fmt.Errorf("парсинг ответа: %w", err)
	}
	return &out, nil, 0, nil
}

// backoff — экспоненциальная задержка с джиттером.
func (c *Client) backoff(attempt int) time.Duration {
	d := c.backoffBase
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= c.backoffMax {
			d = c.backoffMax
			break
		}
	}
	return d + time.Duration(rand.Int63n(int64(250*time.Millisecond)))
}

// retryable — можно ли повторять запрос.
// 429 (rate limit) и 5xx (перегрузка прокси/апстрима) — да; остальные 4xx — нет.
func retryable(ae *apiError, err error) bool {
	if ae != nil {
		return ae.Status == 429 || ae.Status >= 500
	}
	return true // сетевые ошибки всегда ретраебельны
}

// isRetryableNet классифицирует сетевую ошибку как ретраебельную.
// Дедлайн самого запроса НЕ ретраим: caller контролирует общий бюджет через ctx.
func isRetryableNet(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true // таймауты транспорта
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	return false // POST уже отправлен и ответ потерян? — errgroup сам решит
}

// Summarize — вспомогательный вызов для компактизации истории (маленький max_tokens).
func (c *Client) Summarize(ctx context.Context, text string) (string, error) {
	resp, err := c.SendMessage(ctx,
		"Ты — компрессор контекста DevOps-агента. Сожми историю диалога в компактный handoff: сохрани факты, решения, открытые задачи, состояния систем. Без воды, максимум 300 слов, язык — русский.",
		[]Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: text}}}},
		nil)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, b := range resp.Content {
		if b.Type == "text" {
			sb.WriteString(b.Text)
		}
	}
	return sb.String(), nil
}
