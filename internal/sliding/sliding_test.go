package sliding

import (
	"testing"
	"time"
)

func TestWindowBasicAndAdjust(t *testing.T) {
	now := time.Unix(100, 0)
	w := New(time.Minute)
	r := w.Add(now, 70)
	w.Add(now, 20)
	if got := w.Used(now); got != 90 {
		t.Fatal(got)
	}
	w.Adjust(now, r, 10)
	if got := w.Used(now); got != 30 {
		t.Fatal(got)
	}
	if w.Count(now) != 2 {
		t.Fatal(w.Count(now))
	}
	if got := w.WaitForRoom(now, 40, 70); got != 0 {
		t.Fatal(got)
	}
}
func TestWindowSlotsLimitOne(t *testing.T) {
	now := time.Unix(0, 0)
	w := New(time.Minute)
	w.Add(now, 0)
	if d := w.WaitForSlots(now, 1); d != time.Minute {
		t.Fatal(d)
	}
}
func TestWindowAbsorbAndExpiry(t *testing.T) {
	now := time.Unix(0, 0)
	w := New(time.Minute)
	w.Absorb(now, []Entry{{At: now.Add(-time.Second), N: 5}})
	if w.Used(now) != 5 {
		t.Fatal("absorb")
	}
	if w.Used(now.Add(time.Minute)) != 0 {
		t.Fatal("expiry")
	}
}
func TestWindowReferenceModel(t *testing.T) {
	base := time.Unix(0, 0)
	w := New(time.Minute)
	type pair struct {
		at time.Time
		n  int
	}
	var a []pair
	for i := 0; i < 1500; i++ {
		now := base.Add(time.Duration(i) * time.Second)
		n := (i * 37) % 200
		w.Add(now, n)
		a = append(a, pair{now, n})
		sum := 0
		count := 0
		for _, e := range a {
			if e.at.After(now.Add(-time.Minute)) {
				sum += e.n
				count++
			}
		}
		if got := w.Used(now); got != sum {
			t.Fatalf("step %d used=%d want=%d", i, got, sum)
		}
		if got := w.Count(now); got != count {
			t.Fatalf("count=%d want=%d", got, count)
		}
	}
}
func BenchmarkWindowAdd(b *testing.B) {
	w := New(time.Minute)
	now := time.Unix(0, 0)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		w.Add(now.Add(time.Duration(i)*time.Millisecond), 1)
	}
}

func TestAdjustExpiredZeroDoesNotCreateSlot(t *testing.T) {
	start := time.Unix(0, 0)
	w := New(time.Minute)
	ref := w.Add(start, 1)
	w.Adjust(start.Add(time.Minute), ref, 0)
	if got := w.Count(start.Add(time.Minute)); got != 0 {
		t.Fatalf("expired release left %d slots", got)
	}
}

func BenchmarkWaitForRoom(b *testing.B) {
	base := time.Unix(1_000, 0)
	w := New(time.Minute)
	for i := 0; i < 1_000; i++ {
		w.Add(base, 2)
	}
	now := base.Add(time.Second)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = w.WaitForRoom(now, 100, 1_000)
	}
}

func TestCalendarDayExpiresAtLocalMidnight(t *testing.T) {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, time.November, 13, 23, 30, 0, 0, loc)
	w := NewCalendarDay(loc)
	w.Add(start, 1)
	beforeReset := time.Date(2026, time.November, 13, 23, 59, 0, 0, loc)
	if got := w.Used(beforeReset); got != 1 {
		t.Fatalf("usage before reset=%d, want 1", got)
	}
	if got := w.WaitForSlots(beforeReset, 1); got != time.Minute {
		t.Fatalf("wait before reset=%s, want 1m", got)
	}
	afterReset := time.Date(2026, time.November, 14, 0, 0, 0, 0, loc)
	if got := w.Used(afterReset); got != 0 {
		t.Fatalf("usage after reset=%d, want 0", got)
	}
	if got := w.WaitForSlots(afterReset, 1); got != 0 {
		t.Fatalf("wait after reset=%s, want 0", got)
	}
}

func TestCalendarDayUsesDSTAwareNextMidnight(t *testing.T) {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	// Na noite em que o horário de verão termina, a janela diária tem 25 horas.
	start := time.Date(2026, time.November, 1, 0, 30, 0, 0, loc)
	w := NewCalendarDay(loc)
	w.Add(start, 1)
	reset := time.Date(2026, time.November, 2, 0, 0, 0, 0, loc)
	wait := w.WaitForSlots(start, 1)
	want := reset.Sub(start)
	if wait != want {
		t.Fatalf("wait=%s, want DST-aware duration %s", wait, want)
	}
}
