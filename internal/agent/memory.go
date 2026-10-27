package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Entry — запись долговременной памяти.
type Entry struct {
	Kind string    `json:"kind"` // fact | episode | handoff | skill
	Text string    `json:"text"`
	Tags []string  `json:"tags,omitempty"`
	Time time.Time `json:"time"`
}

// Memory — долговременная память агента (JSONL, append-only + полнотекстовый recall).
// Слои по мотивам Hermes Agent (2026): рабочая память (Session), эпизодическая
// (записи Episode), семантическая/навыки (skills-каталог, см. tools/knowledge).
type Memory struct {
	dir   string
	limit int
	log   *slog.Logger

	mu      sync.RWMutex
	entries []Entry
}

// NewMemory открывает (или создаёт) хранилище памяти в dir.
func NewMemory(dir string, limit int, log *slog.Logger) (*Memory, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("memory dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "skills"), 0o755); err != nil {
		return nil, fmt.Errorf("skills dir: %w", err)
	}
	m := &Memory{dir: dir, limit: limit, log: log}
	m.load()
	return m, nil
}

// Dir возвращает каталог памяти.
func (m *Memory) Dir() string { return m.dir }

// SkillsDir возвращает каталог навыков (SKILL.md-подобные документы).
func (m *Memory) SkillsDir() string { return filepath.Join(m.dir, "skills") }

func (m *Memory) file() string { return filepath.Join(m.dir, "memory.jsonl") }

// load читает JSONL в индекс (битые строки пропускаются с warning-ом).
func (m *Memory) load() {
	f, err := os.Open(m.file())
	if err != nil {
		if !os.IsNotExist(err) {
			m.log.Warn("memory: не читается индекс", "err", err)
		}
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil || strings.TrimSpace(e.Text) == "" {
			continue
		}
		m.entries = append(m.entries, e)
	}
	m.log.Info("memory: загружено записей", "count", len(m.entries))
}

// Save добавляет запись (пишется синхронно — память дороже скорости).
// При выходе за лимит (×1.2 гистерезис) файл компактируется: переписывается
// последними limit записями — иначе Recall молча «забывал» старое, а файл рос вечно.
func (m *Memory) Save(kind, text string, tags ...string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	e := Entry{Kind: kind, Text: strings.TrimSpace(text), Tags: tags, Time: time.Now().UTC()}

	m.mu.Lock()
	defer m.mu.Unlock()
	f, err := os.OpenFile(m.file(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return fmt.Errorf("memory append: %w", err)
	}
	line, _ := json.Marshal(e)
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	m.entries = append(m.entries, e)
	if m.limit > 0 && len(m.entries) > m.limit+m.limit/5 {
		if err := m.compactLocked(); err != nil {
			m.log.Warn("memory: компакция не удалась (попробую в следующий раз)", "err", err)
		} else {
			m.log.Info("memory: файл скомпактирован", "entries", len(m.entries))
		}
	}
	return nil
}

// compactLocked переписывает memory.jsonl последними limit записями (под m.mu).
func (m *Memory) compactLocked() error {
	if m.limit <= 0 || len(m.entries) <= m.limit {
		return nil
	}
	keep := append([]Entry(nil), m.entries[len(m.entries)-m.limit:]...)
	tmp := m.file() + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, e := range keep {
		line, _ := json.Marshal(e)
		w.Write(line)
		w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil { // это и есть fsync: power loss не съедает память
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, m.file()); err != nil {
		os.Remove(tmp)
		return err
	}
	m.entries = keep
	return nil
}

// Recall ищет релевантные записи по запросу (ключевое совпадение, scoring по частоте).
func (m *Memory) Recall(query string, k int) []Entry {
	if k <= 0 {
		k = 8
	}
	tokens := tokenize(query)
	if len(tokens) == 0 {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	type scored struct {
		e     Entry
		score float64
	}
	var out []scored
	for _, e := range m.entries {
		hay := strings.ToLower(e.Text + " " + strings.Join(e.Tags, " ") + " " + e.Kind)
		var sc float64
		for _, t := range tokens {
			c := float64(strings.Count(hay, t))
			if c > 0 {
				sc += 1 + 0.25*(c-1) // насыщение повторов
			}
		}
		if sc > 0 {
			// свежесть: записи моложе 7 дней получают бонус ×1.2
			if time.Since(e.Time) < 7*24*time.Hour {
				sc *= 1.2
			}
			out = append(out, scored{e, sc})
		}
	}
	// сортировка вставкой (k маленькое, записей немного — без sort.Slice экономим аллокации)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].score > out[j-1].score; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	if len(out) > k {
		out = out[:k]
	}
	res := make([]Entry, len(out))
	for i, s := range out {
		res[i] = s.e
	}
	return res
}

// tokenize — нормализация запроса в токены (ru/en, без стоп-слов).
func tokenize(s string) []string {
	stop := map[string]bool{
		"и": true, "в": true, "на": true, "с": true, "по": true, "для": true, "как": true, "что": true,
		"the": true, "a": true, "of": true, "to": true, "in": true, "is": true, "for": true, "on": true,
	}
	var toks []string
	var cur strings.Builder
	flush := func() {
		t := strings.ToLower(cur.String())
		cur.Reset()
		if len(t) >= 2 && !stop[t] {
			toks = append(toks, t)
		}
	}
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_':
			cur.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return toks
}
