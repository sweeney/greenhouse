package climate

import (
	"strings"
	"testing"
	"time"
)

// TestCountBuckets_AgreesWithAxis is the correctness contract for the cheap
// bucket counter: for any in-cap window it must return exactly
// len(BucketStarts(...)), so the cap check and the materialized axis can never
// disagree. Covers fixed and calendar intervals, including across a DST boundary.
func TestCountBuckets_AgreesWithAxis(t *testing.T) {
	loc := mustLondon(t)

	mk := func(start, stop time.Time) Window {
		return Window{Start: start, Stop: stop, Label: WindowCustom}
	}

	cases := []struct {
		name string
		win  Window
		iv   Interval
	}{
		{"1h over a day", mk(utc(2026, 6, 11, 0, 0), utc(2026, 6, 12, 0, 0)), mustInterval(t, "1h")},
		{"5m over 3h", mk(utc(2026, 6, 11, 0, 0), utc(2026, 6, 11, 3, 0)), mustInterval(t, "5m")},
		{"6h over 2 days", mk(utc(2026, 6, 11, 0, 0), utc(2026, 6, 13, 0, 0)), mustInterval(t, "6h")},
		{"1d calendar over a week", mk(utc(2026, 6, 8, 0, 0), utc(2026, 6, 15, 0, 0)), mustInterval(t, "1d")},
		{"non-aligned 1h", mk(utc(2026, 6, 11, 0, 30), utc(2026, 6, 11, 5, 10)), mustInterval(t, "1h")},
		// Spring-forward week (23h day) — calendar stepping must still match.
		{"1d across spring DST", mk(utc(2026, 3, 27, 0, 0), utc(2026, 4, 2, 0, 0)), mustInterval(t, "1d")},
		// Autumn-back week (25h day).
		{"1d across autumn DST", mk(utc(2026, 10, 23, 0, 0), utc(2026, 10, 29, 0, 0)), mustInterval(t, "1d")},
		{"1h across autumn DST", mk(utc(2026, 10, 25, 0, 0), utc(2026, 10, 26, 0, 0)), mustInterval(t, "1h")},
	}
	for _, c := range cases {
		want := len(BucketStarts(c.win, c.iv, loc))
		if got := countBuckets(c.win, c.iv, loc); got != want {
			t.Errorf("%s: countBuckets = %d, want len(BucketStarts) = %d", c.name, got, want)
		}
	}
}

// TestResolveInterval_HugeCustomWindowCheap reproduces the DoS: a 200-year
// custom window at 5m. Before the fix the cap check built the entire bucket
// axis (~21M time.Time, ~500MB) just to take its length and reject it. The
// counter must reject it WITHOUT materializing the axis — proven by asserting
// zero allocations in the count for the fixed interval.
func TestResolveInterval_HugeCustomWindowCheap(t *testing.T) {
	loc := mustLondon(t)
	win := Window{
		Start: time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC),
		Stop:  time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC),
		Label: WindowCustom,
	}
	iv := mustInterval(t, "5m")

	if _, err := ResolveInterval(win, "5m", loc); err == nil {
		t.Fatal("200-year window at 5m must exceed the bucket cap")
	} else if !strings.Contains(err.Error(), "exceeding the cap") {
		t.Errorf("error should mention the cap: %v", err)
	}

	// The fixed-interval count is O(1) arithmetic: it must allocate nothing.
	if allocs := testing.AllocsPerRun(100, func() {
		_ = countBuckets(win, iv, loc)
	}); allocs != 0 {
		t.Errorf("countBuckets allocated %v times for a 200-year window; the axis must not be materialized", allocs)
	}
}

// TestResolveInterval_HugeCalendarWindowEarlyExits checks the calendar branch
// bounds its walk: counting 1d buckets over 200 years must stop shortly after
// the cap rather than stepping through ~73k days.
func TestResolveInterval_HugeCalendarWindowEarlyExits(t *testing.T) {
	loc := mustLondon(t)
	win := Window{
		Start: time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC),
		Stop:  time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC),
		Label: WindowCustom,
	}
	n := countBuckets(win, mustInterval(t, "1d"), loc)
	if n <= MaxBuckets {
		t.Fatalf("200-year 1d count = %d, want > MaxBuckets", n)
	}
	if n > MaxBuckets+1 {
		t.Errorf("calendar count walked past the cap: got %d, want early exit at MaxBuckets+1 (%d)", n, MaxBuckets+1)
	}
}

// TestResolveWindow_CustomSpanCapped is the defense-in-depth guard: an absurd
// custom span is rejected at window resolution with an intelligible message,
// before any interval logic runs.
func TestResolveWindow_CustomSpanCapped(t *testing.T) {
	loc := mustLondon(t)
	now := utc(2026, 6, 11, 12, 0)
	from := time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)

	if _, err := ResolveWindow(now, loc, WindowCustom, from, to); err == nil {
		t.Fatal("200-year custom window should be rejected by the span cap")
	} else if !strings.Contains(err.Error(), "maximum") {
		t.Errorf("error should mention the maximum span: %v", err)
	}

	// A window within the cap (well under 2 years) is still accepted.
	okFrom := utc(2026, 1, 1, 0, 0)
	okTo := utc(2026, 6, 1, 0, 0)
	if _, err := ResolveWindow(now, loc, WindowCustom, okFrom, okTo); err != nil {
		t.Errorf("a 5-month window must remain valid: %v", err)
	}
}

// mustInterval looks up an allowed interval by token or fails.
func mustInterval(t *testing.T, token string) Interval {
	t.Helper()
	iv, ok := lookupInterval(token)
	if !ok {
		t.Fatalf("interval %q not allowed", token)
	}
	return iv
}
