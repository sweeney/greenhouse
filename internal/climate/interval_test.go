package climate

import (
	"strings"
	"testing"
	"time"
)

func TestDefaultInterval(t *testing.T) {
	loc := mustLondon(t)
	now := time.Date(2026, 6, 11, 14, 0, 0, 0, loc)

	cases := []struct {
		spec string
		from time.Time
		to   time.Time
		want string
	}{
		{spec: WindowToday, want: "1h"},
		{spec: WindowWeek, want: "1d"},
		{spec: WindowMonth, want: "1d"},
		{spec: WindowCustom, from: now, to: now.Add(24 * time.Hour), want: "1h"},
		{spec: WindowCustom, from: now, to: now.Add(10 * 24 * time.Hour), want: "6h"},
		{spec: WindowCustom, from: now, to: now.Add(60 * 24 * time.Hour), want: "1d"},
	}
	for _, c := range cases {
		win, err := ResolveWindow(now, loc, c.spec, c.from, c.to)
		if err != nil {
			t.Fatalf("ResolveWindow(%s): %v", c.spec, err)
		}
		if got := DefaultInterval(win); got != c.want {
			t.Errorf("DefaultInterval(%s span) = %q, want %q", c.spec, got, c.want)
		}
	}
}

func TestResolveIntervalDefaultsWhenEmpty(t *testing.T) {
	loc := mustLondon(t)
	now := time.Date(2026, 6, 11, 14, 0, 0, 0, loc)
	win, _ := ResolveWindow(now, loc, WindowToday, time.Time{}, time.Time{})

	iv, err := ResolveInterval(win, "", loc)
	if err != nil {
		t.Fatalf("ResolveInterval empty: %v", err)
	}
	if iv.Token != "1h" {
		t.Errorf("default today interval = %q, want 1h", iv.Token)
	}
}

func TestResolveIntervalAllowedAndDisallowed(t *testing.T) {
	loc := mustLondon(t)
	now := time.Date(2026, 6, 11, 14, 0, 0, 0, loc)
	win, _ := ResolveWindow(now, loc, WindowToday, time.Time{}, time.Time{})

	for _, tok := range []string{"5m", "15m", "30m", "1h", "6h", "1d"} {
		iv, err := ResolveInterval(win, tok, loc)
		if err != nil {
			t.Errorf("interval %q should be allowed: %v", tok, err)
		}
		if iv.Token != tok {
			t.Errorf("token = %q, want %q", iv.Token, tok)
		}
	}

	for _, bad := range []string{"2m", "1m", "12h", "1w", "nonsense"} {
		if _, err := ResolveInterval(win, bad, loc); err == nil {
			t.Errorf("interval %q should be rejected", bad)
		}
	}
}

func TestResolveIntervalCalendarFlag(t *testing.T) {
	loc := mustLondon(t)
	now := time.Date(2026, 6, 11, 14, 0, 0, 0, loc)
	win, _ := ResolveWindow(now, loc, WindowWeek, time.Time{}, time.Time{})

	day, err := ResolveInterval(win, "1d", loc)
	if err != nil {
		t.Fatalf("ResolveInterval 1d: %v", err)
	}
	if !day.Calendar {
		t.Error("1d interval should be Calendar")
	}

	hour, _ := ResolveInterval(win, "1h", loc)
	if hour.Calendar {
		t.Error("1h interval should NOT be Calendar")
	}
	if hour.Duration != time.Hour {
		t.Errorf("1h Duration = %v", hour.Duration)
	}
}

func TestResolveIntervalMaxBucketsCap(t *testing.T) {
	loc := mustLondon(t)
	now := time.Date(2026, 6, 11, 0, 0, 0, 0, loc)
	// 60-day custom window at 5m → 17280 buckets, well over the cap.
	from := now
	to := now.Add(60 * 24 * time.Hour)
	win, err := ResolveWindow(now, loc, WindowCustom, from, to)
	if err != nil {
		t.Fatalf("ResolveWindow custom: %v", err)
	}

	_, err = ResolveInterval(win, "5m", loc)
	if err == nil {
		t.Fatal("5m over 60 days should exceed MaxBuckets")
	}
	if !strings.Contains(err.Error(), "exceeding the cap") {
		t.Errorf("error should mention the cap: %v", err)
	}
	if !strings.Contains(err.Error(), "coarser interval") {
		t.Errorf("error should suggest a coarser interval: %v", err)
	}

	if _, err := ResolveInterval(win, "1d", loc); err != nil {
		t.Errorf("1d over 60 days should be within cap: %v", err)
	}
}

func TestResolveIntervalJustUnderCap(t *testing.T) {
	loc := mustLondon(t)
	now := time.Date(2026, 6, 11, 0, 0, 0, 0, loc)
	// 3 days at 5m = 864 buckets (< cap): allowed.
	win, _ := ResolveWindow(now, loc, WindowCustom, now, now.Add(3*24*time.Hour))
	if _, err := ResolveInterval(win, "5m", loc); err != nil {
		t.Errorf("864 buckets should be under cap: %v", err)
	}
}

// TestResolveInterval_SevenDayFiveMinute is the reason MaxBuckets was raised
// from 1000: a 7-day window at 5-minute resolution must resolve rather than
// 400. It covers all three ways a caller expresses "a week": an exact 168h
// custom range (2016 buckets), the period-to-date `week` window near its
// widest, and the `7d` rolling window straddling the autumn DST change (a 25h
// day pushes the count above a naive 7*24*12). The month-at-5m case still
// exceeds the cap, proving the guard is preserved, not removed.
func TestResolveInterval_SevenDayFiveMinute(t *testing.T) {
	loc := mustLondon(t)
	iv := mustInterval(t, "5m")

	// Exact 168h custom window: 7*24*12 = 2016 buckets.
	custom, err := ResolveWindow(utc(2026, 6, 1, 0, 0), loc, WindowCustom,
		utc(2026, 6, 1, 0, 0), utc(2026, 6, 8, 0, 0))
	if err != nil {
		t.Fatalf("ResolveWindow custom: %v", err)
	}
	if n := countBuckets(custom, iv, loc); n != 2016 {
		t.Errorf("168h at 5m = %d buckets, want 2016", n)
	}
	if _, err := ResolveInterval(custom, "5m", loc); err != nil {
		t.Errorf("7-day custom window at 5m must resolve: %v", err)
	}

	// Period-to-date `week` near its widest: now late on a Sunday.
	weekNow := time.Date(2026, 6, 14, 23, 55, 0, 0, loc) // Sunday
	week, err := ResolveWindow(weekNow, loc, WindowWeek, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("ResolveWindow week: %v", err)
	}
	if _, err := ResolveInterval(week, "5m", loc); err != nil {
		t.Errorf("week window at 5m must resolve: %v", err)
	}

	// `7d` rolling across the autumn DST change (clocks go back 2026-10-25), so
	// the real span exceeds 168h and the bucket count climbs above 2016. It must
	// still be within the raised cap.
	dstNow := time.Date(2026, 10, 27, 23, 55, 0, 0, loc)
	dst, ok, err := resolveRolling(dstNow, loc, "7d")
	if err != nil || !ok {
		t.Fatalf("resolveRolling 7d: ok=%v err=%v", ok, err)
	}
	if n := countBuckets(dst, iv, loc); n <= 2016 || n > MaxBuckets {
		t.Errorf("7d across autumn DST at 5m = %d buckets, want in (2016, %d]", n, MaxBuckets)
	}
	if _, err := ResolveInterval(dst, "5m", loc); err != nil {
		t.Errorf("7d rolling window at 5m must resolve: %v", err)
	}

	// Guard preserved: a month at 5m (~8640 buckets) still exceeds the cap.
	month, _ := ResolveWindow(utc(2026, 6, 1, 0, 0), loc, WindowCustom,
		utc(2026, 6, 1, 0, 0), utc(2026, 7, 1, 0, 0))
	if _, err := ResolveInterval(month, "5m", loc); err == nil {
		t.Error("a month at 5m must still exceed the cap")
	}
}

func TestAllowedIntervals(t *testing.T) {
	got := AllowedIntervals()
	want := []string{"5m", "15m", "30m", "1h", "6h", "1d"}
	if len(got) != len(want) {
		t.Fatalf("AllowedIntervals = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("AllowedIntervals[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if len(sortedTokens()) != len(want) {
		t.Errorf("sortedTokens len mismatch")
	}
}

func TestResolveIntervalNilLocation(t *testing.T) {
	if _, err := ResolveInterval(Window{}, "1h", nil); err == nil {
		t.Fatal("nil location should error")
	}
}
