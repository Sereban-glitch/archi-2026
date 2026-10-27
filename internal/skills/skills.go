// Package skills — само-пишущиеся навыки агента (паттерн Hermes Agent:
// AIAgent self-improvement + Claude Code auto-memory). Навык — markdown-файл
// в memory/skills/<name>.md c шапкой:
//
//	# Заголовок навыка
//	tags: postgres, backup
//
//	Тело: пошаговая процедура, команды, предупреждения.
//
// Агент сохраняет навыки сам (tool skill_save) после удачного решения,
// а релевантные навыки автоматически подмешиваются в системный промпт.
package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Doc — один навык.
type Doc struct {
	Name  string   // имя файла без .md (a-z0-9-_)
	Title string   // первая строка «# …»
	Tags  []string // строка «tags: a, b»
	Body  string   // markdown-тело
}

// Skills — потокобезопасный каталог навыков с ленивой загрузкой.
type Skills struct {
	dir    string
	mu     sync.RWMutex
	docs   map[string]Doc
	loaded bool
}

// New создаёт каталог (сам каталог на диске создаётся при первом сохранении).
func New(dir string) *Skills {
	return &Skills{dir: dir, docs: make(map[string]Doc)}
}

// Dir возвращает каталог навыков.
func (s *Skills) Dir() string {
	if s == nil {
		return ""
	}
	return s.dir
}

var nameRe = regexp.MustCompile(`[^a-z0-9-_]+`)

// SafeName нормализует имя навыка в валидное имя файла.
func SafeName(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	n = nameRe.ReplaceAllString(n, "-")
	n = strings.Trim(n, "-_")
	if n == "" {
		return "skill"
	}
	if len(n) > 64 {
		n = n[:64]
	}
	return n
}

// loadOnce лениво читает *.md из каталога (битые файлы пропускаются).
func (s *Skills) loadOnce() {
	if s == nil || s.loaded {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded {
		return
	}
	s.loaded = true
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return // каталога ещё нет — это нормально
	}
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, ent.Name()))
		if err != nil {
			continue
		}
		name := strings.TrimSuffix(ent.Name(), ".md")
		if doc, ok := parseDoc(string(data)); ok {
			doc.Name = name
			s.docs[name] = doc
		}
	}
}

// parseDoc разбирает markdown навыка: «# Title», опц. «tags: …», тело.
func parseDoc(raw string) (Doc, bool) {
	lines := strings.Split(raw, "\n")
	var d Doc
	i := 0
	// пропускаем пустые строки до заголовка
	for ; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "" {
			break
		}
	}
	if i >= len(lines) || !strings.HasPrefix(strings.TrimSpace(lines[i]), "# ") {
		return d, false // без заголовка — не навык
	}
	d.Title = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[i]), "# "))
	i++
	// теги сразу после заголовка
	for ; i < len(lines); i++ {
		l := strings.TrimSpace(lines[i])
		if strings.HasPrefix(l, "tags:") {
			for _, t := range strings.Split(strings.TrimPrefix(l, "tags:"), ",") {
				if t = strings.TrimSpace(t); t != "" {
					d.Tags = append(d.Tags, t)
				}
			}
			continue
		}
		break
	}
	d.Body = strings.TrimSpace(strings.Join(lines[i:], "\n"))
	return d, true
}

// Save записывает навык на диск (атомарно: tmp + rename) и обновляет кэш.
func (s *Skills) Save(d Doc) error {
	if s == nil {
		return fmt.Errorf("skills: каталог не настроен")
	}
	d.Name = SafeName(d.Name)
	if strings.TrimSpace(d.Title) == "" {
		return fmt.Errorf("skills: title обязателен")
	}
	if strings.TrimSpace(d.Body) == "" {
		return fmt.Errorf("skills: body пуст")
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("skills: создать каталог: %w", err)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s\n", d.Title)
	if len(d.Tags) > 0 {
		fmt.Fprintf(&sb, "tags: %s\n", strings.Join(d.Tags, ", "))
	}
	fmt.Fprintf(&sb, "\n%s\n", strings.TrimSpace(d.Body))

	path := filepath.Join(s.dir, d.Name+".md")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(sb.String()), 0o640); err != nil {
		return fmt.Errorf("skills: запись: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("skills: rename: %w", err)
	}

	s.mu.Lock()
	s.docs[d.Name] = d
	s.loaded = true // после ручного Save не перетираем кэш повторной загрузкой
	s.mu.Unlock()
	return nil
}

// Get возвращает навык по имени.
func (s *Skills) Get(name string) (Doc, bool) {
	if s == nil {
		return Doc{}, false
	}
	s.loadOnce()
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.docs[SafeName(name)]
	return d, ok
}

// List возвращает имена всех навыков (отсортированные).
func (s *Skills) List() []string {
	if s == nil {
		return nil
	}
	s.loadOnce()
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.docs))
	for name := range s.docs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Search ищет топ-k навыков по запросу: заголовок ×3 + теги ×2 + тело ×1.
func (s *Skills) Search(query string, k int) []Doc {
	if s == nil || k <= 0 {
		return nil
	}
	tokens := tokenizeSearch(query)
	if len(tokens) == 0 {
		return nil
	}
	s.loadOnce()
	s.mu.RLock()
	defer s.mu.RUnlock()

	type scored struct {
		d Doc
		s float64
	}
	var out []scored
	for _, d := range s.docs {
		title := strings.ToLower(d.Title)
		tags := strings.ToLower(strings.Join(d.Tags, " "))
		body := strings.ToLower(d.Body)
		var sc float64
		for _, t := range tokens {
			if strings.Contains(title, t) {
				sc += 3
			}
			if strings.Contains(tags, t) {
				sc += 2
			}
			if strings.Contains(body, t) {
				sc += 1
			}
		}
		if sc >= 3 { // хотя бы попадание в заголовок или 3 по телу/тегам
			out = append(out, scored{d, sc})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].s > out[j].s })
	if len(out) > k {
		out = out[:k]
	}
	docs := make([]Doc, len(out))
	for i, o := range out {
		docs[i] = o.d
	}
	return docs
}

// tokenizeSearch — слова запроса ≥2 символов, нижний регистр.
func tokenizeSearch(q string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(strings.ToLower(q), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'а' && r <= 'я' || r >= '0' && r <= '9' || r == '_')
	}) {
		if len(f) >= 2 {
			out = append(out, f)
		}
	}
	return out
}
