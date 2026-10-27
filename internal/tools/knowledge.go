package tools

import (
        "context"
        "encoding/json"
        "fmt"
        "os"
        "path/filepath"
        "strings"
        "time"
)

// registerKnowledge — долговременная память и каталог навыков (SKILL.md).
// Паттерн из Hermes Agent (2026): трёхслойная память — рабочая (сессия),
// эпизодическая (memory_save/recall) и семантическая (skills-каталог).
func (r *Registry) registerKnowledge() {
        r.Register(&Tool{
                Name: "memory_recall",
                Description: "Найти в долговременной памяти факты/эпизоды по запросу. Используй ПЕРЕД ответом " +
                        "на вопросы о прошлых событиях, решениях и настройках инфраструктуры.",
                Timeout: 10 * time.Second,
                Schema: map[string]any{
                        "type": "object",
                        "properties": map[string]any{
                                "query": map[string]any{"type": "string", "description": "поисковый запрос по ключевым словам"},
                        },
                        "required": []string{"query"},
                },
                Exec: r.execMemoryRecall,
        })
        r.Register(&Tool{
                Name: "memory_save",
                Description: "Сохранить важный факт/эпизод в долговременную память (автоматически подгружается в будущих сессиях). " +
                        "Сохраняй: решённые инциденты и их причины, изменения конфигурации, пароли-НЕ-тут, привычки пользователя.",
                Timeout: 10 * time.Second,
                Schema: map[string]any{
                        "type": "object",
                        "properties": map[string]any{
                                "kind": map[string]any{"type": "string", "enum": []string{"fact", "episode", "skill"},
                                        "description": "fact=устойчивый факт, episode=событие, skill=how-to процедура"},
                                "text": map[string]any{"type": "string"},
                                "tags": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
                        },
                        "required": []string{"text"},
                },
                Exec: r.execMemorySave,
        })
        r.Register(&Tool{
                Name: "search_knowledge",
                Description: "Поиск по локальной базе знаний: каталог skills/ (SKILL.md-документы) и заметки. " +
                        "Формат вызова: query — что искать. Возвращает имена файлов и совпавшие строки.",
                Timeout: 20 * time.Second,
                Schema: map[string]any{
                        "type": "object",
                        "properties": map[string]any{
                                "query": map[string]any{"type": "string"},
                        },
                        "required": []string{"query"},
                },
                Exec: r.execSearchKnowledge,
        })
}

func (r *Registry) execMemoryRecall(ctx context.Context, args json.RawMessage) (string, bool) {
        q := argString(args, "query", "")
        if q == "" {
                return "ОШИБКА: query обязателен", true
        }
        entries := r.deps.Memory.Recall(q, r.deps.Cfg.Memory.RecallLimit)
        if len(entries) == 0 {
                return "Ничего не найдено в долговременной памяти по запросу: " + q, false
        }
        var sb strings.Builder
        fmt.Fprintf(&sb, "Найдено %d записей:\n", len(entries))
        for i, e := range entries {
                fmt.Fprintf(&sb, "%d. [%s | %s | %s] %s\n", i+1, e.Kind,
                        e.Time.Format("2006-01-02 15:04"), strings.Join(e.Tags, ","), clip(e.Text, 400))
        }
        return sb.String(), false
}

func (r *Registry) execMemorySave(ctx context.Context, args json.RawMessage) (string, bool) {
        text := argString(args, "text", "")
        if text == "" {
                return "ОШИБКА: text обязателен", true
        }
        kind := argString(args, "kind", "fact")
        var tags []string
        if len(args) > 0 {
                var m map[string]any
                if err := json.Unmarshal(args, &m); err == nil {
                        if arr, ok := m["tags"].([]any); ok {
                                for _, v := range arr {
                                        if s, ok := v.(string); ok {
                                                tags = append(tags, s)
                                        }
                                }
                        }
                }
        }
        if err := r.deps.Memory.Save(kind, text, tags...); err != nil {
                return fmt.Sprintf("ОШИБКА записи памяти: %v", err), true
        }
        return "OK: запись сохранена в долговременную память", false
}

func (r *Registry) execSearchKnowledge(ctx context.Context, args json.RawMessage) (string, bool) {
        q := argString(args, "query", "")
        if q == "" {
                return "ОШИБКА: query обязателен", true
        }
        dir := r.deps.Memory.SkillsDir()
        var sb strings.Builder
        tokens := tokenizeQuery(q)
        if len(tokens) == 0 {
                return "ОШИБКА: пустой запрос", true
        }

        _ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
                if err != nil || d.IsDir() {
                        return nil
                }
                if !strings.HasSuffix(path, ".md") && !strings.HasSuffix(path, ".txt") {
                        return nil
                }
                data, err := os.ReadFile(path)
                if err != nil {
                        return nil
                }
                lower := strings.ToLower(string(data))
                var hits int
                for _, t := range tokens {
                        if strings.Contains(lower, t) {
                                hits++
                        }
                }
                if hits == 0 {
                        return nil
                }
                fmt.Fprintf(&sb, "📄 %s (совпадений: %d)\n", path, hits)
                // первая содержательная строка как превью
                for _, line := range strings.Split(string(data), "\n") {
                        if l := strings.TrimSpace(line); strings.HasPrefix(l, "#") || (len(l) > 30 && hits > 0) {
                                sb.WriteString("   " + clip(l, 160) + "\n")
                                break
                        }
                }
                return nil
        })
        if sb.Len() == 0 {
                return "В базе знаний ничего не найдено. Добавь документы в " + dir + " (SKILL.md-формат).", false
        }
        return "Результаты поиска в базе знаний:\n" + sb.String(), false
}

func tokenizeQuery(s string) []string {
        var out []string
        for _, f := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
                return !(r >= 'a' && r <= 'z' || r >= 'а' && r <= 'я' || r >= '0' && r <= '9' || r == '_')
        }) {
                if len(f) >= 2 && f != "и" && f != "the" && f != "для" {
                        out = append(out, f)
                }
        }
        return out
}

func clip(s string, n int) string {
        if len(s) <= n {
                return s
        }
        return s[:n] + "…"
}
