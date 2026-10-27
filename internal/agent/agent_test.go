package agent

import "testing"

// TestTruncateRuneSafe — truncate не рвёт UTF-8 (кириллица = 2 байта/руна).
func TestTruncateRuneSafe(t *testing.T) {
	in := []byte("привет мир") // 19 байт: 9 рун + пробел (каждая руна 2 байта) = 18+1
	out := truncate(in, 5)
	if out == "" {
		t.Fatal("пустой результат")
	}
	// 5 байт вмещают 2 двухбайтные руны («пр»), третья не влезает целиком
	if want := "пр…"; out != want {
		t.Errorf("truncate = %q, хочу %q", out, want)
	}
}

func TestTruncateShort(t *testing.T) {
	if got := truncate([]byte("abc"), 10); got != "abc" {
		t.Errorf("короткий вход изменился: %q", got)
	}
}
