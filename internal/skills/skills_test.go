package skills

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSaveGetSearch — полный цикл: сохранить → найти по запросу → прочитать.
func TestSaveGetSearch(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)

	if err := s.Save(Doc{
		Name:  "postgres-backup",
		Title: "Резервное копирование PostgreSQL",
		Tags:  []string{"postgres", "backup"},
		Body:  "pg_dump archi > /backup/archi.sql && gzip -9 /backup/archi.sql",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(Doc{
		Name:  "vm-low-ram",
		Title: "Что делать при нехватке RAM на agrobot-prod",
		Tags:  []string{"memory", "vm"},
		Body:  "Проверить top, прибить тяжёлые процессы, перезапустить console-bot",
	}); err != nil {
		t.Fatal(err)
	}

	// поиск по смыслу ключевых слов: «postgres» в заголовке и тегах
	hits := s.Search("как бэкапить postgres", 2)
	if len(hits) == 0 || hits[0].Name != "postgres-backup" {
		t.Fatalf("поиск не нашёл навык про postgres: %+v", hits)
	}

	// точное чтение
	d, ok := s.Get("postgres-backup")
	if !ok || d.Title != "Резервное копирование PostgreSQL" {
		t.Fatalf("Get вернул мусор: %+v ok=%v", d, ok)
	}
	if len(d.Tags) != 2 || d.Tags[0] != "postgres" {
		t.Fatalf("теги не распарсились: %v", d.Tags)
	}
	if filepath.Base(d.Name) != "postgres-backup" {
		t.Fatalf("имя испорчено: %q", d.Name)
	}

	// файл на диске в формате навыка
	raw, err := os.ReadFile(filepath.Join(dir, "postgres-backup.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw[:2]) != "# " {
		t.Fatalf("файл начинается не с заголовка: %q", string(raw[:20]))
	}
}

// TestSearchRanking — заголовок весит больше тела; слабые попадания отфильтрованы.
func TestSearchRanking(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	_ = s.Save(Doc{Name: "a", Title: "Настройка nginx на Mint", Body: "конфиг в /etc/nginx/nginx.conf"})
	_ = s.Save(Doc{Name: "b", Title: "Сборка ядра Linux", Body: "иногда упоминают nginx мимоходом"})
	hits := s.Search("nginx", 2)
	// b имеет только слабое попадание по телу (sc=1 < порога 3) — отфильтрован
	if len(hits) != 1 || hits[0].Name != "a" {
		t.Fatalf("заголовочное попадание должно побеждать, слабые — отфильтровываться: %+v", hits)
	}
}

// TestSafeName — имена навыков санитизируются.
func TestSafeName(t *testing.T) {
	cases := map[string]string{
		"Postgres Backup!!": "postgres-backup",
		"../../etc/passwd":  "etc-passwd",
		"":                  "skill",
		"РУССКОЕ-ИМЯ":       "skill", // не-латиница вырезается целиком
	}
	for in, want := range cases {
		if got := SafeName(in); got != want {
			t.Errorf("SafeName(%q) = %q, хочу %q", in, got, want)
		}
	}
}

// TestNilSafe — nil-каталог не паникует (skills выключены в конфиге).
func TestNilSafe(t *testing.T) {
	var s *Skills
	if s.Dir() != "" {
		t.Error("Dir на nil")
	}
	if _, ok := s.Get("x"); ok {
		t.Error("Get на nil вернул true")
	}
	if s.List() != nil {
		t.Error("List на nil вернул список")
	}
	if s.Search("x", 2) != nil {
		t.Error("Search на nil вернул результаты")
	}
}

// TestParseDocBadFormat — файл без «# заголовка» игнорируется.
func TestParseDocBadFormat(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "broken.md"), []byte("просто текст без заголовка"), 0o640); err != nil {
		t.Fatal(err)
	}
	s := New(dir)
	if got := s.List(); len(got) != 0 {
		t.Errorf("битый файл не должен попадать в каталог: %v", got)
	}
}
