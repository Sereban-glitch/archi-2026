package agent

import (
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Session — диалоговое состояние одного клиента (Telegram-чат или CLI-сессия).
// Потокобезопасна: append/history под мьютексом, компактизация — атомарной заменой слайса.
type Session struct {
	ID       string
	mu       sync.Mutex
	history  []Message
	lastUsed time.Time
	hints    []string // подсказки памяти (обновляются при создании сессии)
}

// Append добавляет сообщение в историю.
func (s *Session) Append(m Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = append(s.history, m)
	s.lastUsed = time.Now()
}

// History возвращает копию истории (для запроса к LLM).
func (s *Session) History() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Message, len(s.history))
	copy(out, s.history)
	return out
}

// ReplaceHistory атомарно заменяет историю (компактизация).
func (s *Session) ReplaceHistory(h []Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = h
	s.lastUsed = time.Now()
}

// Chars — суммарный объём истории в символах (метрика для компактизации).
func (s *Session) Chars() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, m := range s.history {
		for _, b := range m.Content {
			n += len(b.Text) + len(b.Content) + len(b.Input)
		}
	}
	return n
}

// LastText — последний текстовый ответ ассистента (для graceful fallback).
func (s *Session) LastText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.history) - 1; i >= 0; i-- {
		if s.history[i].Role != "assistant" {
			continue
		}
		var sb strings.Builder
		for _, b := range s.history[i].Content {
			if b.Type == "text" && b.Text != "" {
				sb.WriteString(b.Text)
				sb.WriteString("\n")
			}
		}
		if t := strings.TrimSpace(sb.String()); t != "" {
			return t
		}
	}
	return ""
}

// SessionManager — реестр сессий с LRU-вытеснением простаивающих.
type SessionManager struct {
	mu       sync.Mutex
	sessions map[string]*Session
	limit    int
	ttl      time.Duration
	log      *slog.Logger
}

// NewSessionManager создаёт менеджер.
func NewSessionManager(limit int, ttl time.Duration, log *slog.Logger) *SessionManager {
	if limit <= 0 {
		limit = 50
	}
	return &SessionManager{sessions: make(map[string]*Session), limit: limit, ttl: ttl, log: log}
}

// Get возвращает (или создаёт) сессию по ID.
func (m *SessionManager) Get(id string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[id]; ok {
		return s
	}
	m.evictLocked()
	s := &Session{ID: id, lastUsed: time.Now()}
	m.sessions[id] = s
	return s
}

// Reset сбрасывает сессию (полезно после «зависшего» диалога).
func (m *SessionManager) Reset(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
}

// evictLocked вытесняет простаивающие сессии (вызывается под локом).
func (m *SessionManager) evictLocked() {
	if len(m.sessions) < m.limit {
		return
	}
	cutoff := time.Now().Add(-m.ttl)
	for id, s := range m.sessions {
		if s.lastUsed.Before(cutoff) {
			delete(m.sessions, id)
		}
	}
	// Если всё ещё много — выбрасываем самые старые.
	for len(m.sessions) >= m.limit {
		var oldestID string
		var oldest time.Time
		first := true
		for id, s := range m.sessions {
			if first || s.lastUsed.Before(oldest) {
				oldestID, oldest, first = id, s.lastUsed, false
			}
		}
		if oldestID == "" {
			return
		}
		delete(m.sessions, oldestID)
		m.log.Info("session: вытеснена старая сессия", "id", oldestID)
	}
}
