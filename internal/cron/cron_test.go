package cron

import (
	"testing"
	"time"
)

// TestNextSlotDaily — ближайший слот ищется и сегодня, и завтра.
// Регрессия P1-аудита: вечером 22:00 следующий слот — завтра 09:00, не +24ч.
func TestNextSlotDaily(t *testing.T) {
	tz := time.FixedZone("test", 3*3600)
	now := time.Date(2026, 10, 27, 22, 0, 0, 0, tz)
	times := []string{"09:00", "21:00"}

	got, ok := NextSlot(now, times, 0, tz)
	if !ok {
		t.Fatal("слот не найден")
	}
	want := time.Date(2026, 10, 28, 9, 0, 0, 0, tz)
	if !got.Equal(want) {
		t.Errorf("22:00 → следующий %v, хочу %v (завтра 09:00)", got, want)
	}

	// утро: слот сегодня
	now = time.Date(2026, 10, 27, 8, 0, 0, 0, tz)
	got, ok = NextSlot(now, times, 0, tz)
	if !ok {
		t.Fatal("слот не найден")
	}
	want = time.Date(2026, 10, 27, 9, 0, 0, 0, tz)
	if !got.Equal(want) {
		t.Errorf("08:00 → следующий %v, хочу %v (сегодня 09:00)", got, want)
	}

	// ровно в слот — берём следующий (полуоткрытый интервал After)
	now = time.Date(2026, 10, 27, 9, 0, 0, 0, tz)
	got, _ = NextSlot(now, times, 0, tz)
	want = time.Date(2026, 10, 27, 21, 0, 0, 0, tz)
	if !got.Equal(want) {
		t.Errorf("09:00 ровно → %v, хочу сегодня 21:00", got)
	}
}

// TestNextSlotEvery — интервальный режим.
func TestNextSlotEvery(t *testing.T) {
	tz := time.UTC
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, tz)
	got, ok := NextSlot(now, nil, 6*time.Hour, tz)
	if !ok {
		t.Fatal("интервал не посчитан")
	}
	if !got.Equal(now.Add(6 * time.Hour)) {
		t.Errorf("every 6h → %v, хочу %v", got, now.Add(6*time.Hour))
	}
}

// TestNextSlotInvalid — пустое расписание = false.
func TestNextSlotInvalid(t *testing.T) {
	if _, ok := NextSlot(time.Now(), nil, 0, time.UTC); ok {
		t.Error("без times и every слота быть не должно")
	}
}
