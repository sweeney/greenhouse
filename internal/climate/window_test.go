package climate

import (
	"testing"
	"time"

	"github.com/sweeney/greenhouse/internal/testutil"
)

// mustLondon loads Europe/London or fails the test.
func mustLondon(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatalf("LoadLocation(Europe/London): %v", err)
	}
	return loc
}

// utc builds a UTC instant.
func utc(y int, mo time.Month, d, h, mi int) time.Time {
	return time.Date(y, mo, d, h, mi, 0, 0, time.UTC)
}

// resolve drives the resolver via a FakeClock to prove no time.Now() is used.
func resolve(t *testing.T, now time.Time, loc *time.Location, spec string, from, to time.Time) Window {
	t.Helper()
	clk := testutil.NewFakeClock(now)
	w, err := ResolveWindow(clk.Now(), loc, spec, from, to)
	if err != nil {
		t.Fatalf("ResolveWindow(%q): unexpected error: %v", spec, err)
	}
	return w
}

func TestResolveWindow_TodayWeekMonth(t *testing.T) {
	loc := mustLondon(t)
	// 2026-06-11 is a Thursday. In June, London is BST (UTC+1), so 14:30 UTC
	// is 15:30 local.
	now := utc(2026, 6, 11, 14, 30)

	tests := []struct {
		spec  string
		start time.Time
		label string
	}{
		{WindowToday, time.Date(2026, 6, 11, 0, 0, 0, 0, loc), WindowToday},
		{WindowWeek, time.Date(2026, 6, 8, 0, 0, 0, 0, loc), WindowWeek},
		{WindowMonth, time.Date(2026, 6, 1, 0, 0, 0, 0, loc), WindowMonth},
	}

	for _, tc := range tests {
		t.Run(tc.spec, func(t *testing.T) {
			w := resolve(t, now, loc, tc.spec, time.Time{}, time.Time{})
			if !w.Start.Equal(tc.start) {
				t.Errorf("Start = %s, want %s", w.Start, tc.start)
			}
			if !w.Stop.Equal(now) {
				t.Errorf("Stop = %s, want now %s", w.Stop, now)
			}
			if w.Label != tc.label {
				t.Errorf("Label = %q, want %q", w.Label, tc.label)
			}
		})
	}
}

func TestResolveWindow_WeekStartsMonday(t *testing.T) {
	loc := mustLondon(t)
	wantMonday := time.Date(2026, 6, 8, 0, 0, 0, 0, loc)

	tests := []struct {
		name string
		now  time.Time
	}{
		{"monday", utc(2026, 6, 8, 9, 0)},
		{"wednesday", utc(2026, 6, 10, 9, 0)},
		{"sunday", utc(2026, 6, 14, 20, 0)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := resolve(t, tc.now, loc, WindowWeek, time.Time{}, time.Time{})
			if !w.Start.Equal(wantMonday) {
				t.Errorf("week Start = %s, want %s", w.Start, wantMonday)
			}
		})
	}
}

func TestResolveWindow_BSTvsGMTOffset(t *testing.T) {
	loc := mustLondon(t)

	summer := resolve(t, utc(2026, 7, 15, 10, 0), loc, WindowToday, time.Time{}, time.Time{})
	_, summerOffset := summer.Start.Zone()
	if summerOffset != 3600 {
		t.Errorf("summer Start offset = %ds, want 3600 (BST UTC+1)", summerOffset)
	}
	if got := summer.Start.UTC(); !got.Equal(utc(2026, 7, 14, 23, 0)) {
		t.Errorf("summer Start in UTC = %s, want 2026-07-14 23:00Z", got)
	}

	winter := resolve(t, utc(2026, 1, 15, 10, 0), loc, WindowToday, time.Time{}, time.Time{})
	_, winterOffset := winter.Start.Zone()
	if winterOffset != 0 {
		t.Errorf("winter Start offset = %ds, want 0 (GMT UTC+0)", winterOffset)
	}
	if got := winter.Start.UTC(); !got.Equal(utc(2026, 1, 15, 0, 0)) {
		t.Errorf("winter Start in UTC = %s, want 2026-01-15 00:00Z", got)
	}

	if summerOffset == winterOffset {
		t.Errorf("expected BST and GMT offsets to differ, both = %d", summerOffset)
	}
}

func TestResolveWindow_DSTTransitionDays(t *testing.T) {
	loc := mustLondon(t)

	dayDuration := func(now time.Time) time.Duration {
		w, err := ResolveWindow(now, loc, WindowToday, time.Time{}, time.Time{})
		if err != nil {
			t.Fatalf("ResolveWindow: %v", err)
		}
		ln := w.Start.In(loc)
		nextMidnight := time.Date(ln.Year(), ln.Month(), ln.Day()+1, 0, 0, 0, 0, loc)
		return nextMidnight.Sub(w.Start)
	}

	t.Run("spring forward 23h day", func(t *testing.T) {
		// 2026-03-29: last Sunday in March, clocks go forward -> 23h day.
		now := utc(2026, 3, 29, 22, 0)
		if d := dayDuration(now); d != 23*time.Hour {
			t.Errorf("spring-forward day length = %v, want 23h", d)
		}
	})

	t.Run("autumn back 25h day", func(t *testing.T) {
		// 2026-10-25: last Sunday in October, clocks go back -> 25h day.
		now := utc(2026, 10, 25, 22, 0)
		if d := dayDuration(now); d != 25*time.Hour {
			t.Errorf("autumn-back day length = %v, want 25h", d)
		}
	})
}

func TestWindow_Days(t *testing.T) {
	loc := mustLondon(t)

	t.Run("custom full days", func(t *testing.T) {
		from := utc(2026, 6, 1, 0, 0)
		to := utc(2026, 6, 4, 0, 0)
		w, err := ResolveWindow(utc(2026, 6, 11, 0, 0), loc, WindowCustom, from, to)
		if err != nil {
			t.Fatalf("ResolveWindow: %v", err)
		}
		if got := w.Days(); got != 3 {
			t.Errorf("Days() = %v, want 3", got)
		}
	})

	t.Run("fractional period-to-date", func(t *testing.T) {
		now := utc(2026, 1, 15, 6, 0) // GMT: local midnight is 00:00 UTC
		w := resolve(t, now, loc, WindowToday, time.Time{}, time.Time{})
		if got := w.Days(); got != 0.25 {
			t.Errorf("Days() = %v, want 0.25", got)
		}
	})
}

func TestResolveWindow_CustomErrors(t *testing.T) {
	loc := mustLondon(t)
	now := utc(2026, 6, 11, 12, 0)
	good := utc(2026, 6, 10, 0, 0)

	tests := []struct {
		name     string
		spec     string
		from, to time.Time
	}{
		{"missing from", WindowCustom, time.Time{}, good},
		{"missing to", WindowCustom, good, time.Time{}},
		{"missing both", WindowCustom, time.Time{}, time.Time{}},
		{"to equals from", WindowCustom, good, good},
		{"to before from", WindowCustom, good, good.Add(-time.Hour)},
		{"unknown spec", "fortnight", time.Time{}, time.Time{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ResolveWindow(now, loc, tc.spec, tc.from, tc.to); err == nil {
				t.Errorf("expected error, got nil")
			}
		})
	}
}

func TestResolveWindow_RollingDays(t *testing.T) {
	loc := mustLondon(t)
	now := utc(2026, 6, 11, 14, 30) // Thursday, BST

	cases := []struct {
		spec  string
		start time.Time
	}{
		{"1d", time.Date(2026, 6, 11, 0, 0, 0, 0, loc)},  // ≡ today
		{"7d", time.Date(2026, 6, 5, 0, 0, 0, 0, loc)},   // today + previous 6 days
		{"30d", time.Date(2026, 5, 13, 0, 0, 0, 0, loc)}, // crosses the month boundary
	}
	for _, tc := range cases {
		t.Run(tc.spec, func(t *testing.T) {
			w := resolve(t, now, loc, tc.spec, time.Time{}, time.Time{})
			if !w.Start.Equal(tc.start) {
				t.Errorf("Start = %s, want %s", w.Start, tc.start)
			}
			if !w.Stop.Equal(now) {
				t.Errorf("Stop = %s, want now %s", w.Stop, now)
			}
			if w.Label != tc.spec {
				t.Errorf("Label = %q, want %q", w.Label, tc.spec)
			}
			// Day-form Start must be local midnight.
			if h, m, s := w.Start.In(loc).Clock(); h != 0 || m != 0 || s != 0 {
				t.Errorf("Start %s is not local midnight", w.Start)
			}
		})
	}
}

// The motivating case: on a Monday, week-to-date collapses to today, but a
// rolling 7d still looks back a full week.
func TestResolveWindow_RollingDayWeekdayIndependent(t *testing.T) {
	loc := mustLondon(t)
	now := utc(2026, 6, 15, 9, 0) // 2026-06-15 is a Monday

	today := resolve(t, now, loc, WindowToday, time.Time{}, time.Time{})
	week := resolve(t, now, loc, WindowWeek, time.Time{}, time.Time{})
	if !week.Start.Equal(today.Start) {
		t.Fatalf("on a Monday week.Start (%s) should coincide with today.Start (%s)", week.Start, today.Start)
	}

	seven := resolve(t, now, loc, "7d", time.Time{}, time.Time{})
	if seven.Start.Equal(today.Start) {
		t.Errorf("7d.Start must differ from today.Start on a Monday, both %s", seven.Start)
	}
	want := time.Date(2026, 6, 9, 0, 0, 0, 0, loc) // midnight 6 days back
	if !seven.Start.Equal(want) {
		t.Errorf("7d.Start = %s, want %s", seven.Start, want)
	}
}

func TestResolveWindow_RollingHours(t *testing.T) {
	loc := mustLondon(t)
	now := utc(2026, 6, 11, 14, 30)
	w := resolve(t, now, loc, "24h", time.Time{}, time.Time{})
	if !w.Start.Equal(now.Add(-24 * time.Hour)) {
		t.Errorf("24h Start = %s, want now-24h %s", w.Start, now.Add(-24*time.Hour))
	}
	if !w.Stop.Equal(now) {
		t.Errorf("Stop = %s, want now %s", w.Stop, now)
	}
	if w.Label != "24h" {
		t.Errorf("Label = %q, want 24h", w.Label)
	}
}

// A day-form rolling window across the spring-forward boundary spans a 23h day,
// so the elapsed time is one hour short of the naive day count.
func TestResolveWindow_RollingDSTSpringForward(t *testing.T) {
	loc := mustLondon(t)
	// 2026-03-29 is the 23h spring-forward day. now = 11:00 BST on the 30th.
	now := utc(2026, 3, 30, 10, 0)
	w := resolve(t, now, loc, "2d", time.Time{}, time.Time{})
	wantStart := time.Date(2026, 3, 29, 0, 0, 0, 0, loc)
	if !w.Start.Equal(wantStart) {
		t.Errorf("2d Start = %s, want %s", w.Start, wantStart)
	}
	if got := w.Stop.Sub(w.Start); got != 34*time.Hour {
		t.Errorf("2d elapsed = %v, want 34h (23h DST day + 11h), not 35h", got)
	}
}

func TestResolveWindow_RollingOverCap(t *testing.T) {
	loc := mustLondon(t)
	now := utc(2026, 6, 11, 12, 0)
	for _, spec := range []string{"100000d", "100000h"} {
		if _, err := ResolveWindow(now, loc, spec, time.Time{}, time.Time{}); err == nil {
			t.Errorf("%s: expected over-cap error, got nil", spec)
		}
	}
}

// Malformed rolling-ish specs fall through to the unknown-spec error.
func TestResolveWindow_RollingInvalid(t *testing.T) {
	loc := mustLondon(t)
	now := utc(2026, 6, 11, 12, 0)
	for _, spec := range []string{"0d", "d", "h", "-5d", "7x", "7", "7days", "1.5d"} {
		if _, err := ResolveWindow(now, loc, spec, time.Time{}, time.Time{}); err == nil {
			t.Errorf("%q: expected error, got nil", spec)
		}
	}
}

func TestResolveWindow_NilLocation(t *testing.T) {
	if _, err := ResolveWindow(utc(2026, 6, 11, 12, 0), nil, WindowToday, time.Time{}, time.Time{}); err == nil {
		t.Errorf("expected error for nil location, got nil")
	}
}
