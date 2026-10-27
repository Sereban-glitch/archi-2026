// embed.go — векторизация текста для семантического recall памяти.
// OpenAI-совместимый POST {base_url}/embeddings: работает с OpenAI,
// локальными серверами эмбеддингов и LLM-прокси, которые его проксируют.
// Вектор хранится прямо в записи memory.jsonl (поле vec) — компакция
// памяти сохраняет его автоматически.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/archi/archi/internal/config"
)

// Embedder — контракт векторизатора (для тестов — детерминированные фейки).
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float64, error)
}

// HTTPEmbedder — реализация через OpenAI-совместимый /embeddings.
type HTTPEmbedder struct {
	base  string
	key   string
	model string
	http  *http.Client
}

// NewHTTPEmbedder создаёт HTTP-векторизатор. cfg должен быть провалидирован.
func NewHTTPEmbedder(cfg config.EmbeddingsConfig) *HTTPEmbedder {
	timeout := cfg.Timeout.Duration
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &HTTPEmbedder{
		base:  strings.TrimRight(cfg.BaseURL, "/"),
		key:   cfg.APIKey,
		model: cfg.Model,
		http:  &http.Client{Timeout: timeout},
	}
}

// Embed векторизует пакет текстов (батчем — один запрос на всё).
func (e *HTTPEmbedder) Embed(ctx context.Context, texts []string) ([][]float64, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	payload, err := json.Marshal(map[string]any{"model": e.model, "input": texts})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.base+"/embeddings", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	if e.key != "" {
		req.Header.Set("Authorization", "Bearer "+e.key)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embeddings сеть: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("embeddings чтение: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embeddings http %d: %s", resp.StatusCode, clipStr(string(raw), 200))
	}

	var out struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("embeddings парсинг: %w", err)
	}
	if len(out.Data) != len(texts) {
		return nil, fmt.Errorf("embeddings: ждал %d векторов, пришло %d", len(texts), len(out.Data))
	}
	vecs := make([][]float64, len(out.Data))
	for i, d := range out.Data {
		if len(d.Embedding) == 0 {
			return nil, fmt.Errorf("embeddings: пустой вектор #%d", i)
		}
		vecs[i] = d.Embedding
	}
	return vecs, nil
}

// cosine — косинусная близость двух векторов (1 = идентичны, 0 = ортогональны).
func cosine(a, b []float64) float64 {
	n := len(a)
	if n == 0 || len(b) != n {
		return 0
	}
	var dot, na, nb float64
	for i := 0; i < n; i++ {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func clipStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
