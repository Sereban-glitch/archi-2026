package metrics

import (
	"strings"
	"testing"
)

// TestIncAndRender — счётчики с лейблами рендерятся в Prometheus text format.
func TestIncAndRender(t *testing.T) {
	Reset()
	Inc("archi_tool_calls_total", "tool", "bash", "result", "ok")
	Inc("archi_tool_calls_total", "tool", "bash", "result", "ok")
	Inc("archi_tool_calls_total", "tool", "bash", "result", "error")

	out := Render()
	want := []string{
		`archi_tool_calls_total{result="error",tool="bash"} 1`,
		`archi_tool_calls_total{result="ok",tool="bash"} 2`,
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("в рендере нет строки %q:\n%s", w, out)
		}
	}
}

// TestAdd — накопление величин (токены).
func TestAdd(t *testing.T) {
	Reset()
	Add("archi_llm_tokens_total", 150, "direction", "in")
	Add("archi_llm_tokens_total", 50, "direction", "in")
	out := Render()
	if !strings.Contains(out, `archi_llm_tokens_total{direction="in"} 200`) {
		t.Errorf("Add не просуммировал: %s", out)
	}
}

// TestUnpairedLabels — нечётное число лейблов не паникует.
func TestUnpairedLabels(t *testing.T) {
	Reset()
	Inc("archi_weird_total", "orphan")
	out := Render()
	if !strings.Contains(out, "archi_weird_total") {
		t.Errorf("нечётные лейблы потеряли метрику: %q", out)
	}
}

// TestGauge — вычисляемая метрика вызывается при рендере.
func TestGauge(t *testing.T) {
	Reset()
	n := 0
	RegisterGauge("archi_test_gauge", func() float64 { n++; return float64(n) })
	_ = Render()
	_ = Render()
	if n != 2 {
		t.Errorf("gauge должен вызываться на каждый рендер, вызван %d раз", n)
	}
}
