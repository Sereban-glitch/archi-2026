// browser.go — CDP-браузерный инструмент (GAP-аудит: plane 8 из 9).
// Открывает страницу в headless Chromium/Chrome, ждёт полной загрузки,
// извлекает текст (JS-рендеринг, SPA) и опционально сохраняет скриншот.
//
// Конструкция single-shot: каждый вызов поднимает чистый браузер и убивает
// его после — нет состояния между вызовами, нет утечек процессов-зомби.
// Для потоков «логин → клик → клик» (2FA/takeover) инструмент сознательно
// не предназначен: интерактивный просмотр остаётся за владельцем.
package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/archi/archi/internal/config"

	"golang.org/x/net/websocket"
)

func (r *Registry) registerBrowser() {
	r.Register(&Tool{
		Name: "browser",
		Description: "Открыть страницу в headless-браузере (Chromium): выполнить JS-рендеринг и вернуть текст страницы. " +
			"Опционально сохраняет скриншот в JPEG для владельца. Используй для SPA-дашбордов, статус-страниц и сайтов, " +
			"где curl видит пустой каркас. Требует установленный chromium (см. browser.chrome_bin).",
		Timeout: r.deps.Cfg.Browser.Timeout.Duration,
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url": map[string]any{"type": "string", "description": "http(s):// адрес страницы"},
				"wait_seconds": map[string]any{"type": "integer",
					"description": "дополнительное ожидание JS-отрисовки после загрузки (0-30, default 3)"},
				"screenshot": map[string]any{"type": "boolean", "description": "сохранить JPEG-скриншот (default false)"},
			},
			"required": []string{"url"},
		},
		Exec: r.execBrowser,
	})
}

var urlRe = regexp.MustCompile(`^https?://[^\s]+$`)

func (r *Registry) execBrowser(ctx context.Context, args json.RawMessage) (string, bool) {
	cfg := r.deps.Cfg.Browser
	url := argString(args, "url", "")
	if url == "" {
		return "ОШИБКА: url обязателен", true
	}
	if !urlRe.MatchString(url) {
		return "ОШИБКА: url должен начинаться с http:// или https://", true
	}
	wait := argInt(args, "wait_seconds", 3)
	if wait < 0 {
		wait = 0
	}
	if wait > 30 {
		wait = 30
	}
	wantShot := false
	if len(args) > 0 {
		var m map[string]any
		if err := json.Unmarshal(args, &m); err == nil {
			wantShot, _ = m["screenshot"].(bool)
		}
	}

	bin := findChrome(cfg.ChromeBin)
	if bin == "" {
		return "ОШИБКА: браузер не найден. Установи: sudo apt install chromium-browser (или укажи browser.chrome_bin)", true
	}

	dataDir := r.deps.Cfg.DataDir
	text, shotPath, err := browse(ctx, cfg, bin, url, time.Duration(wait)*time.Second, wantShot, dataDir)
	if err != nil {
		return fmt.Sprintf("ОШИБКА браузера: %v", err), true
	}

	limit := cfg.MaxTextBytes
	if limit <= 0 {
		limit = 262_144
	}
	if len(text) > limit {
		text = text[:limit] + "\n…[текст страницы урезан]"
	}
	out := "=== ТЕКСТ СТРАНИЦЫ " + url + " ===\n" + text
	if shotPath != "" {
		out += "\n\nСкриншот сохранён: " + shotPath
	}
	return out, false
}

// findChrome ищет бинарь браузера: явный путь из конфига или авто-детект.
func findChrome(configured string) string {
	if configured != "" {
		if p, err := exec.LookPath(configured); err == nil {
			return p
		}
		return ""
	}
	for _, cand := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "chrome"} {
		if p, err := exec.LookPath(cand); err == nil {
			return p
		}
	}
	return ""
}

// browse — полный single-shot цикл: старт chrome → WS → navigate → текст (→ скриншот) → kill.
// dataDir передаётся явно: каталог для скриншотов <data_dir>/browser/.
func browse(ctx context.Context, cfg config.BrowserConfig, bin, url string, extraWait time.Duration, wantShot bool, dataDir string) (string, string, error) {
	// Свободный порт для DevTools.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", "", fmt.Errorf("нет свободного порта: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	profile, err := os.MkdirTemp("", "archi-chrome-*")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(profile)

	args := []string{
		"--headless=new",
		"--disable-gpu",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-extensions",
		"--window-size=1280,900",
		"--user-data-dir=" + profile,
		"--remote-debugging-address=127.0.0.1",
		"--remote-debugging-port=" + strconv.Itoa(port),
		"about:blank",
	}
	if cfg.Headless != nil && !*cfg.Headless {
		args = args[1:] // убираем headless (редкий случай отладки на ноутбуке)
	}
	cmd := exec.Command(bin, args...)
	var stderr syncBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", "", fmt.Errorf("старт %s: %w", bin, err)
	}
	kill := func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_, _ = cmd.Process.Wait()
	}
	defer kill()

	// Ждём DevTools endpoint (Chrome стартует не мгновенно).
	var wsURL string
	for i := 0; i < 40; i++ { // до 10 секунд
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
		var v struct {
			WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
		}
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", port))
		if err != nil {
			continue
		}
		body := json.NewDecoder(resp.Body)
		_ = body.Decode(&v)
		resp.Body.Close()
		if v.WebSocketDebuggerURL != "" {
			wsURL = v.WebSocketDebuggerURL
			break
		}
	}
	if wsURL == "" {
		return "", "", fmt.Errorf("DevTools не поднялся за 10с (stderr: %s)", stderr.Clip(200))
	}

	// CDP-сессия.
	cdp, err := dialCDP(wsURL)
	if err != nil {
		return "", "", fmt.Errorf("ws: %w", err)
	}
	defer cdp.Close()

	// Создаём вкладку и цепляемся.
	var created struct {
		TargetID string `json:"targetId"`
	}
	if err := cdp.call(ctx, "Target.createTarget", map[string]any{"url": "about:blank"}, &created); err != nil {
		return "", "", err
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := cdp.call(ctx, "Target.attachToTarget", map[string]any{
		"targetId": created.TargetID, "flatten": true,
	}, &attached); err != nil {
		return "", "", err
	}
	sess := attached.SessionID

	if err := cdp.call(ctx, "Page.enable", nil, nil, sess); err != nil {
		return "", "", err
	}

	// Подписываемся на load ДО навигации.
	loadCh := cdp.expect("Page.loadEventFired", sess)
	if err := cdp.call(ctx, "Page.navigate", map[string]any{"url": url}, nil, sess); err != nil {
		return "", "", err
	}
	select {
	case <-loadCh:
	case <-time.After(20 * time.Second):
		// не дождались load — всё равно попробуем прочитать DOM
	case <-ctx.Done():
		return "", "", ctx.Err()
	}
	if extraWait > 0 {
		select {
		case <-time.After(extraWait): // даём SPA дорисовать
		case <-ctx.Done():
			return "", "", ctx.Err()
		}
	}

	// Текст страницы.
	var ev struct {
		Result struct {
			Type  string `json:"type"`
			Value string `json:"value"`
		} `json:"result"`
	}
	if err := cdp.call(ctx, "Runtime.evaluate", map[string]any{
		"expression":    "document.readyState + '\\n' + (document.body ? document.body.innerText : '(пусто)')",
		"returnByValue": true,
	}, &ev, sess); err != nil {
		return "", "", err
	}
	text := ev.Result.Value

	shotPath := ""
	if wantShot {
		shotDir := filepath.Join(dataDir, "browser")
		var shot struct {
			Data string `json:"data"`
		}
		if err := cdp.call(ctx, "Page.captureScreenshot", map[string]any{
			"format": "jpeg", "quality": 70,
		}, &shot, sess); err == nil && shot.Data != "" {
			if raw, err := base64.StdEncoding.DecodeString(shot.Data); err == nil {
				if err := os.MkdirAll(shotDir, 0o755); err == nil {
					shotPath = filepath.Join(shotDir,
						fmt.Sprintf("shot-%s.jpg", time.Now().Format("20060102-150405")))
					if err := os.WriteFile(shotPath, raw, 0o640); err != nil {
						shotPath = ""
					}
				}
			}
		}
	}
	return text, shotPath, nil
}

// ---- минимальный CDP-клиент на x/net/websocket ----

type cdpClient struct {
	ws      *websocket.Conn
	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan *cdpMsg
	logMu   sync.Mutex
	// events
	evMu   sync.Mutex
	subs   map[string][]chan struct{}
	closed bool
}

type cdpMsg struct {
	ID     int64           `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
}

func dialCDP(wsURL string) (*cdpClient, error) {
	ws, err := websocket.Dial(wsURL, "", "http://127.0.0.1/")
	if err != nil {
		return nil, err
	}
	c := &cdpClient{ws: ws, pending: make(map[int64]chan *cdpMsg), subs: make(map[string][]chan struct{})}
	go c.readLoop()
	return c, nil
}

// readLoop разносит ответы по pending и события подписчикам.
func (c *cdpClient) readLoop() {
	for {
		var raw string
		if err := websocket.Message.Receive(c.ws, &raw); err != nil {
			c.evMu.Lock()
			c.closed = true
			c.evMu.Unlock()
			return
		}
		var m cdpMsg
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			continue
		}
		if m.ID != 0 {
			c.mu.Lock()
			ch, ok := c.pending[m.ID]
			delete(c.pending, m.ID)
			c.mu.Unlock()
			if ok {
				ch <- &m
			}
			continue
		}
		// событие
		c.evMu.Lock()
		for key, subs := range c.subs {
			if key == m.Method {
				for _, ch := range subs {
					select {
					case ch <- struct{}{}:
					default:
					}
				}
			}
		}
		c.evMu.Unlock()
	}
}

// expect подписывается на событие (до вызова команды, его порождающей).
func (c *cdpClient) expect(method, session string) <-chan struct{} {
	ch := make(chan struct{}, 1)
	c.evMu.Lock()
	c.subs[method] = append(c.subs[method], ch)
	c.evMu.Unlock()
	return ch
}

// call отправляет команду и ждёт результат (распаковывается в out, nil = не нужен).
func (c *cdpClient) call(ctx context.Context, method string, params any, out any, session ...string) error {
	payload := map[string]any{"id": c.allocID(), "method": method}
	if params != nil {
		payload["params"] = params
	}
	if len(session) > 0 && session[0] != "" {
		payload["sessionId"] = session[0]
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	id := int64(payload["id"].(int64))
	ch := make(chan *cdpMsg, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()

	if err := websocket.Message.Send(c.ws, string(body)); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return fmt.Errorf("cdp send %s: %w", method, err)
	}

	select {
	case m := <-ch:
		if m.Error != nil {
			return fmt.Errorf("cdp %s: %s", method, m.Error.Message)
		}
		if out != nil && len(m.Result) > 0 {
			if err := json.Unmarshal(m.Result, out); err != nil {
				return fmt.Errorf("cdp %s: парсинг результата: %w", method, err)
			}
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(30 * time.Second):
		return fmt.Errorf("cdp %s: таймаут", method)
	}
}

func (c *cdpClient) allocID() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	return c.nextID
}

func (c *cdpClient) Close() {
	_ = c.ws.Close()
}

// syncBuffer — потокобезопасный буфер stderr chrome.
type syncBuffer struct {
	mu  sync.Mutex
	buf [4096]byte
	n   int
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.n < len(b.buf) {
		m := copy(b.buf[b.n:], p)
		b.n += m
	}
	return len(p), nil
}

func (b *syncBuffer) Clip(n int) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := string(b.buf[:b.n])
	if len(s) > n {
		s = s[:n]
	}
	return s
}
