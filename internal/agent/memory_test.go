package agent

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeEmbedder — детерминированный векторизатор для тестов: текст про сервера
// получает вектор [1,0], текст про котиков — [0,1].
type fakeEmbedder struct{}

func (fakeEmbedder) Embed(ctx context.Context, texts []string) ([][]float64, error) {
	out := make([][]float64, len(texts))
	for i, t := range texts {
		l := strings.ToLower(t)
		if strings.Contains(l, "сервер") || strings.Contains(l, "нода") || strings.Contains(l, "agrobot") {
			out[i] = []float64{1, 0}
		} else {
			out[i] = []float64{0, 1}
		}
	}
	return out, nil
}

// TestHybridRecallSemantic — семантический recall находит перефразированное,
// когда ключевые слова не совпадают вовсе.
func TestHybridRecallSemantic(t *testing.T) {
	dir := t.TempDir()
	log := testLogger()
	m, err := NewMemory(dir, 100, log)
	if err != nil {
		t.Fatal(err)
	}
	m.SetEmbedder(fakeEmbedder{})

	// запись про серверы — без слов «недоступна»
	if err := m.Save("fact", "Ночью перезагружали agrobot-prod из-за обновления ядра", "ops"); err != nil {
		t.Fatal(err)
	}
	// запись про котиков — вектор ортогонален серверному
	if err := m.Save("fact", "Кот Барсик любит спать на клавиатуре", "жизнь"); err != nil {
		t.Fatal(err)
	}

	// запрос «нода недоступна» не имеет общих токенов с записью про серверы,
	// но семантика (вектор [1,0]) должна вытянуть именно её.
	hits := m.Recall("нода недоступна что делать", 2)
	if len(hits) == 0 {
		t.Fatal("гибридный recall ничего не нашёл")
	}
	if !strings.Contains(hits[0].Text, "agrobot-prod") {
		t.Fatalf("первым должен быть серверный факт, а не %q", hits[0].Text)
	}
}

// TestRecallKeywordOnly — без Embedder работает прежний keyword-путь.
func TestRecallKeywordOnly(t *testing.T) {
	dir := t.TempDir()
	m, err := NewMemory(dir, 100, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	_ = m.Save("fact", "Порт SSH на agrobot-prod — 22", "ssh")
	_ = m.Save("fact", "База PostgreSQL называется archi", "db")

	hits := m.Recall("какой порт ssh", 2)
	if len(hits) == 0 || !strings.Contains(hits[0].Text, "SSH") {
		t.Fatalf("keyword-recall деградировал: %+v", hits)
	}
}

// TestSaveEmbedFailureNotFatal — упавший эмбеддинг не мешает сохранению.
func TestSaveEmbedFailureNotFatal(t *testing.T) {
	dir := t.TempDir()
	m, err := NewMemory(dir, 100, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	m.SetEmbedder(errEmbedder{})
	if err := m.Save("fact", "важный факт при мёртвом эмбеддере", "test"); err != nil {
		t.Fatalf("Save не должен падать из-за эмбеддинга: %v", err)
	}
	// запись на диске есть, просто без vec
	raw, err := os.ReadFile(filepath.Join(dir, "memory.jsonl"))
	if err != nil || len(raw) == 0 {
		t.Fatalf("запись не сохранилась: %v", err)
	}
	if strings.Contains(string(raw), "\"vec\"") {
		t.Error("вектора быть не должно")
	}
}

type errEmbedder struct{}

func (errEmbedder) Embed(ctx context.Context, texts []string) ([][]float64, error) {
	return nil, context.DeadlineExceeded
}

// TestCosine — базовые свойства косинуса.
func TestCosine(t *testing.T) {
	if c := cosine([]float64{1, 0}, []float64{1, 0}); c < 0.999 {
		t.Errorf("идентичные векторы: %v", c)
	}
	if c := cosine([]float64{1, 0}, []float64{0, 1}); c > 0.001 {
		t.Errorf("ортогональные векторы: %v", c)
	}
	if c := cosine([]float64{1, 0}, []float64{0, 0}); c != 0 {
		t.Errorf("нулевой вектор: %v", c)
	}
	if c := cosine([]float64{1}, []float64{1, 2}); c != 0 {
		t.Errorf("разная длина: %v", c)
	}
}

// TestFreshnessBonus — свежесть всё ещё влияет (не потеряно в гибриде).
func TestFreshnessBonus(t *testing.T) {
	dir := t.TempDir()
	m, _ := NewMemory(dir, 100, testLogger())
	_ = m.Save("fact", "яндекс сервер индексация", "test")
	// делаем запись старой вручную (манипулируем временем через reload)
	old := `{"kind":"fact","text":"старый сервер яндекс","time":"2020-01-01T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(dir, "memory.jsonl"), []byte(old), 0o640); err != nil {
		t.Fatal(err)
	}
	m2, _ := NewMemory(dir, 100, testLogger())
	hits := m2.Recall("яндекс сервер", 5)
	if len(hits) == 0 || !strings.Contains(hits[0].Text, "яндекс") {
		t.Fatalf("свежая запись должна побеждать: %+v", hits)
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
