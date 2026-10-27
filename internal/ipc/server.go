// Package ipc — локальный UNIX-сокет сервер: доступ к агенту прямо из терминала
// ноутбука без Telegram. Протокол — line-based JSON (newline-delimited):
//
//	запрос:  {"text":"статус сервера?"}\n
//	ответ:   {"ok":true,"reply":"..."}\n  или  {"ok":false,"error":"..."}\n
package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/archi/archi/internal/agent"
	"github.com/archi/archi/internal/bridge"
)

// Server — UNIX-сокет сервер локального доступа.
type Server struct {
	socketPath string
	agent      *agent.Agent
	bridge     *bridge.Bridge
	log        *slog.Logger

	listener atomic.Pointer[net.Listener]
	mu       sync.Mutex
	closed   atomic.Bool
}

// NewServer создаёт IPC-сервер.
func NewServer(socketPath string, a *agent.Agent, b *bridge.Bridge, log *slog.Logger) *Server {
	return &Server{socketPath: socketPath, agent: a, bridge: b, log: log}
}

// request — строка запроса от CLI-клиента.
type request struct {
	Text   string `json:"text"`
	Reset  bool   `json:"reset,omitempty"`
	Status bool   `json:"status,omitempty"`
	Voice  bool   `json:"voice,omitempty"` // голосовой режим (ear): короткий разговорный ответ без markdown
}

// response — строка ответа.
type response struct {
	OK    bool   `json:"ok"`
	Reply string `json:"reply,omitempty"`
	Error string `json:"error,omitempty"`
}

// Serve запускает сервер (создаёт сокет-файл, принимает подключения).
func (s *Server) Serve(ctx context.Context) error {
	// Каталог сокета (напр. /run/archi) — создаём с правами 0755.
	if err := os.MkdirAll(filepath.Dir(s.socketPath), 0o755); err != nil {
		return fmt.Errorf("ipc: не создан каталог сокета: %w", err)
	}
	// Убираем зависший сокет от прошлой сессии (stale).
	if _, err := os.Stat(s.socketPath); err == nil {
		// проверяем, жив ли старый владелец — попыткой подключения
		if c, err := net.DialTimeout("unix", s.socketPath, 500*time.Millisecond); err == nil {
			c.Close()
			return fmt.Errorf("ipc: сокет %s занят живым процессом archi", s.socketPath)
		}
		s.log.Warn("ipc: удаляю устаревший сокет", "path", s.socketPath)
		if err := os.Remove(s.socketPath); err != nil {
			return fmt.Errorf("ipc: не удалить старый сокет: %w", err)
		}
	}

	ln, err := net.Listen("unix", s.socketPath) // #nosec G302 — каталог под контролем юзера
	if err != nil {
		return fmt.Errorf("ipc: listen %s: %w", s.socketPath, err)
	}
	s.listener.Store(&ln)
	// Права: только владелец.
	_ = os.Chmod(s.socketPath, 0o660)

	go func() {
		<-ctx.Done()
		s.Close()
	}()

	s.log.Info("ipc: слушаю UNIX-сокет", "path", s.socketPath)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if s.closed.Load() || errors.Is(err, net.ErrClosed) {
				return nil
			}
			s.log.Warn("ipc: accept ошибка", "err", err)
			continue
		}
		go s.handleConn(ctx, conn)
	}
}

// Close закрывает слушатель и удаляет сокет-файл.
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Swap(true) {
		return
	}
	if p := s.listener.Load(); p != nil {
		_ = (*p).Close()
	}
	_ = os.Remove(s.socketPath)
}

// handleConn — одна клиентская сессия (line-delimited JSON, несколько запросов на соединение).
func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	// Сессия выбирается на каждый запрос: "cli" для терминала, "ear" для
	// голосового режима (контекст `archi ctl` сохраняется между вызовами,
	// параллельные запросы сериализуются агентом).
	defer func() {
		if rec := recover(); rec != nil {
			s.log.Error("ipc: паника обработчика", "panic", rec)
		}
	}()

	// общий дедлайн на соединение — 15 минут (долгие задачи агента влезают)
	_ = conn.SetDeadline(time.Now().Add(15 * time.Minute))
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)

	for sc.Scan() {
		if ctx.Err() != nil {
			return
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req request
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			writeJSON(conn, response{OK: false, Error: "битый JSON: " + err.Error()})
			continue
		}
		// Голосовой режим (ear, 2026.15) — отдельная сессия: разговорный
		// контекст не смешивается с терминальным `archi ctl`.
		sessionID := "cli"
		if req.Voice {
			sessionID = "ear"
		}
		// микро-команды
		if req.Reset {
			s.agent.Sessions().Reset(sessionID)
			writeJSON(conn, response{OK: true, Reply: "контекст сброшен"})
			continue
		}
		if req.Status {
			writeJSON(conn, response{OK: true, Reply: statusJSON(s.bridge, s.agent)})
			continue
		}
		if strings.TrimSpace(req.Text) == "" {
			writeJSON(conn, response{OK: false, Error: "пустой текст"})
			continue
		}
		// основной путь: агент (голосовой режим — с разговорным промптом)
		actx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		reply, err := s.agent.HandleOpts(actx, sessionID, req.Text, agent.HandleOpts{Voice: req.Voice})
		cancel()
		if err != nil {
			s.log.Error("ipc: ошибка агента", "err", err)
		}
		writeJSON(conn, response{OK: err == nil, Reply: reply, Error: errText(err)})
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func writeJSON(conn net.Conn, r response) {
	b, _ := json.Marshal(r)
	_, _ = conn.Write(append(b, '\n'))
}

// statusJSON — компактный статус узлов для IPC-клиентов.
func statusJSON(b *bridge.Bridge, ag *agent.Agent) string {
	snap := b.Snapshot()
	var sb strings.Builder
	if ag != nil {
		req, inT, outT := ag.UsageStats()
		fmt.Fprintf(&sb, "llm: запросов %d, токенов in %d / out %d\n", req, inT, outT)
	}
	if snap == nil {
		sb.WriteString("телеметрия ещё не собрана\n")
		return sb.String()
	}
	for _, pair := range []struct {
		t string
		m *bridge.NodeMetrics
	}{{"local", snap.Local}, {"remote", snap.Remote}} {
		if pair.m == nil {
			continue
		}
		if !pair.m.Online {
			fmt.Fprintf(&sb, "%s: DOWN (%s)\n", pair.t, pair.m.Error)
			continue
		}
		fmt.Fprintf(&sb, "%s: CPU %.0f%% RAM %.1f%% disk %.0f%% load %.2f\n",
			pair.t, pair.m.CPUPercent, pair.m.MemPercent(), pair.m.DiskPercent(), pair.m.Load1)
	}
	return sb.String()
}
