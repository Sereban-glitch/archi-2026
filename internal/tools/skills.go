package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/archi/archi/internal/skills"
)

// registerSkills — само-пишущиеся навыки (паттерн Hermes self-improvement):
// агент сохраняет удачные процедуры как SKILL.md и переиспользует их
// в будущих сессиях (релевантные подмешиваются в системный промпт).
func (r *Registry) registerSkills() {
	r.Register(&Tool{
		Name: "skill_save",
		Description: "Сохранить навык — проверенную пошаговую процедуру (how-to), чтобы переиспользовать её в будущем. " +
			"Вызывай ПОСЛЕ удачного решения нетривиальной задачи: заголовок = суть, body = шаги и команды. " +
			"Релевантные навыки автоматически подмешиваются в контекст при похожих запросах.",
		Timeout: 10 * time.Second,
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":  map[string]any{"type": "string", "description": "короткое латиницей: postgres-backup"},
				"title": map[string]any{"type": "string", "description": "человеческий заголовок: «Резервное копирование PostgreSQL»"},
				"tags":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "ключевые слова для поиска"},
				"body":  map[string]any{"type": "string", "description": "markdown: шаги, команды, предупреждения"},
			},
			"required": []string{"name", "title", "body"},
		},
		Exec: r.execSkillSave,
	})
	r.Register(&Tool{
		Name: "skill_read",
		Description: "Прочитать сохранённый навык целиком (пошаговую процедуру). " +
			"Список доступных — skill_list; имена из поиска навыков в промпте.",
		Timeout: 10 * time.Second,
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string", "description": "имя навыка из skill_list"},
			},
			"required": []string{"name"},
		},
		Exec: r.execSkillRead,
	})
	r.Register(&Tool{
		Name: "skill_list",
		Description: "Показать все сохранённые навыки (имя + заголовок + теги). " +
			"Полезно перед skill_read, чтобы узнать точное имя.",
		Timeout: 10 * time.Second,
		Schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		Exec: r.execSkillList,
	})
}

func (r *Registry) execSkillSave(ctx context.Context, args json.RawMessage) (string, bool) {
	if r.deps.Skills == nil {
		return "ОШИБКА: навыки выключены (skills.enabled=false)", true
	}
	name := argString(args, "name", "")
	title := argString(args, "title", "")
	body := argString(args, "body", "")
	if name == "" || title == "" || body == "" {
		return "ОШИБКА: name, title и body обязательны", true
	}
	var tags []string
	if len(args) > 0 {
		var m map[string]any
		if err := json.Unmarshal(args, &m); err == nil {
			if arr, ok := m["tags"].([]any); ok {
				for _, v := range arr {
					if s, ok := v.(string); ok && s != "" {
						tags = append(tags, s)
					}
				}
			}
		}
	}
	d := skills.Doc{Name: name, Title: title, Tags: tags, Body: body}
	if err := r.deps.Skills.Save(d); err != nil {
		return fmt.Sprintf("ОШИБКА сохранения навыка: %v", err), true
	}
	return "OK: навык сохранён: " + d.Name + " (в будущих сессиях подмешается автоматически при похожем запросе)", false
}

func (r *Registry) execSkillRead(ctx context.Context, args json.RawMessage) (string, bool) {
	if r.deps.Skills == nil {
		return "ОШИБКА: навыки выключены (skills.enabled=false)", true
	}
	name := argString(args, "name", "")
	if name == "" {
		return "ОШИБКА: name обязателен", true
	}
	d, ok := r.deps.Skills.Get(name)
	if !ok {
		return fmt.Sprintf("Навык %q не найден. Доступны: %v", name, r.deps.Skills.List()), true
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s\n", d.Title)
	if len(d.Tags) > 0 {
		fmt.Fprintf(&sb, "tags: %s\n", strings.Join(d.Tags, ", "))
	}
	sb.WriteString("\n" + d.Body)
	out := sb.String()
	if len(out) > 4000 {
		out = out[:4000] + "\n…[усечено]"
	}
	return out, false
}

func (r *Registry) execSkillList(ctx context.Context, args json.RawMessage) (string, bool) {
	if r.deps.Skills == nil {
		return "ОШИБКА: навыки выключены (skills.enabled=false)", true
	}
	names := r.deps.Skills.List()
	if len(names) == 0 {
		return "Навыков пока нет. Решишь нетривиальную задачу — сохрани процедуру через skill_save.", false
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Навыков: %d\n", len(names))
	for _, n := range names {
		if d, ok := r.deps.Skills.Get(n); ok {
			fmt.Fprintf(&sb, "- %s: %s", n, d.Title)
			if len(d.Tags) > 0 {
				fmt.Fprintf(&sb, " [%s]", strings.Join(d.Tags, ","))
			}
			sb.WriteString("\n")
		}
	}
	return sb.String(), false
}
