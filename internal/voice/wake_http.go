// wake_http.go — wake-word через сайдкар с настоящей моделью (ONNX).
//
// Разведка 2026: индустрия сошлась на openWakeWord-формате моделей
// (ONNX/TFLite, <100 КБ, realtime на CPU):
//   - openwakeword.com — обучение КАСТОМНОГО wake-word («Арчи», любой язык)
//     на их GPU, <1 часа, Apache 2.0 на выходе; им же пользуется Hermes Agent;
//   - livekit-wakeword (github.com/livekit/livekit-wakeword, июнь 2026) —
//     open-source тренировка одной командой локально, модели совместимы
//     с openWakeWord-пайплайном (0.08 ложных срабатываний/час против 8.5).
//
// archi не тянет ONNX в бинарь (cgo убьёт статическую сборку) — вместо этого
// говорим HTTP с деплоем/wake_server.py: POST /wake (WAV 16k) →
// {"detected":bool,"score":float,"keyword":str}. Тот же контракт подойдёт
// любому самописному детектору.
package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// httpWakeResult — ответ сайдкара.
type httpWakeResult struct {
	Detected bool    `json:"detected"`
	Score    float64 `json:"score"`
	Keyword  string  `json:"keyword"`
}

// httpWakeDetect шлёт фразу (WAV) в сайдкар и спрашивает: это wake-word?
func httpWakeDetect(ctx context.Context, url string, wav []byte) (httpWakeResult, error) {
	var res httpWakeResult
	if url == "" {
		return res, fmt.Errorf("wake: wake_server_url пуст")
	}
	reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(wav))
	if err != nil {
		return res, err
	}
	req.Header.Set("Content-Type", "audio/wav")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return res, fmt.Errorf("wake: сайдкар недоступен: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return res, fmt.Errorf("wake: сайдкар ответил %s: %s", resp.Status, string(body))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&res); err != nil {
		return res, fmt.Errorf("wake: сайдкар прислал мусор: %w", err)
	}
	return res, nil
}
