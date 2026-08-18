package climate

import (
	"testing"
	"time"
)

// --- Regression suite for issue #18 -----------------------------------------
//
// The canonical Go bucket axis must sit on the SAME grid Influx's
// aggregateWindow(location:) uses, which is anchored at local midnight in the
// configured zone. When the window start is off that grid — every "<N>h" rolling
// window, and any custom `from` that is not on the interval grid — an unsnapped
// axis can never exact-match an Influx `_start` stamp, so every row snapped back
// into the PRECEDING bucket and the whole series was reported one bucket early.
//
// These tests pin the axis (unit) and then prove the end-to-end behaviour by
// replaying what Influx actually returns (integration).

// localMidnight is the grid anchor: midnight of t's local date in loc.
func localMidnight(t time.Time, loc *time.Location) time.Time {
	lt := t.In(loc)
	return time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, loc)
}

// onGrid reports whether t sits exactly on the iv grid anchored at the local
// midnight of t's own date — the grid Flux's location-aware aggregateWindow uses.
func onGrid(t time.Time, iv Interval, loc *time.Location) bool {
	return t.In(loc).Sub(localMidnight(t, loc))%iv.Duration == 0
}

// --- axis unit tests ---------------------------------------------------------

// TestBucketStarts_RollingHoursSnapToGrid is the headline case from issue #18:
// `window=24h` derives its start from `now`, which carries nanosecond precision
// from the injected clock. The axis must still land on whole hours.
func TestBucketStarts_RollingHoursSnapToGrid(t *testing.T) {
	loc := mustLondon(t)
	// 2026-08-18 11:05:00.453805045 BST — the exact shape of the prod evidence.
	now := time.Date(2026, 8, 18, 11, 5, 0, 453805045, loc)

	win, err := ResolveWindow(now, loc, "24h", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("ResolveWindow: %v", err)
	}
	iv := mustInterval(t, "1h")
	got := BucketStarts(win, iv, loc)

	if len(got) == 0 {
		t.Fatal("no buckets")
	}
	want0 := time.Date(2026, 8, 17, 11, 0, 0, 0, loc)
	if !got[0].Equal(want0) {
		t.Errorf("first bucket = %s, want %s (snapped down to the hour)", got[0], want0)
	}
	for i, b := range got {
		lb := b.In(loc)
		if lb.Minute() != 0 || lb.Second() != 0 || lb.Nanosecond() != 0 {
			t.Errorf("bucket %d = %s: not on a whole hour", i, lb)
		}
	}
	// The window is exactly 24h wide but starts 5m past the hour, so it touches
	// 25 hourly grid cells. Under-counting here is what silently dropped the
	// first partial window before the fix.
	if len(got) != 25 {
		t.Errorf("bucket count = %d, want 25", len(got))
	}
}

// TestBucketStarts_CustomNonAlignedSnapsToGrid ports countinghouse's regression
// for an off-grid custom `from`.
func TestBucketStarts_CustomNonAlignedSnapsToGrid(t *testing.T) {
	loc := mustLondon(t)
	win := Window{
		Start: time.Date(2026, 6, 11, 9, 37, 12, 0, loc),
		Stop:  time.Date(2026, 6, 11, 13, 2, 0, 0, loc),
		Label: WindowCustom,
	}
	got := BucketStarts(win, mustInterval(t, "1h"), loc)

	want := []time.Time{
		time.Date(2026, 6, 11, 9, 0, 0, 0, loc),
		time.Date(2026, 6, 11, 10, 0, 0, 0, loc),
		time.Date(2026, 6, 11, 11, 0, 0, 0, loc),
		time.Date(2026, 6, 11, 12, 0, 0, 0, loc),
		time.Date(2026, 6, 11, 13, 0, 0, 0, loc),
	}
	assertAxis(t, got, want)
}

// TestBucketStarts_CustomNonAlignedCoarse pins the part that makes the anchor
// choice load-bearing: on a BST date a 6h grid must be 00/06/12/18 LOCAL. An
// epoch-anchored (UTC) grid would give 01/07/13/19 local and miss Influx by an
// hour all summer.
func TestBucketStarts_CustomNonAlignedCoarse(t *testing.T) {
	loc := mustLondon(t)
	win := Window{
		Start: time.Date(2026, 6, 11, 9, 37, 12, 0, loc), // BST (UTC+1)
		Stop:  time.Date(2026, 6, 12, 1, 0, 0, 0, loc),
		Label: WindowCustom,
	}
	got := BucketStarts(win, mustInterval(t, "6h"), loc)

	want := []time.Time{
		time.Date(2026, 6, 11, 6, 0, 0, 0, loc),
		time.Date(2026, 6, 11, 12, 0, 0, 0, loc),
		time.Date(2026, 6, 11, 18, 0, 0, 0, loc),
		time.Date(2026, 6, 12, 0, 0, 0, 0, loc),
	}
	assertAxis(t, got, want)
}

// TestBucketStarts_OnGridWindowsUnchanged is the "not affected" guard from the
// issue: today/week/month/<N>d all start at local midnight, which is on every
// sub-day grid, so snapping must be a no-op for them.
func TestBucketStarts_OnGridWindowsUnchanged(t *testing.T) {
	loc := mustLondon(t)
	now := time.Date(2026, 8, 18, 11, 5, 0, 453805045, loc)

	for _, spec := range []string{WindowToday, WindowWeek, WindowMonth, "7d"} {
		for _, tok := range []string{"5m", "15m", "30m", "1h", "6h"} {
			win, err := ResolveWindow(now, loc, spec, time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("ResolveWindow(%s): %v", spec, err)
			}
			iv := mustInterval(t, tok)
			got := BucketStarts(win, iv, loc)
			if len(got) == 0 {
				t.Fatalf("%s/%s: no buckets", spec, tok)
			}
			if !got[0].Equal(win.Start) {
				t.Errorf("%s/%s: first bucket = %s, want the unchanged window start %s",
					spec, tok, got[0], win.Start)
			}
		}
	}
}

// TestBucketStarts_FirstBucketContainsStart is the invariant that makes the snap
// safe: bucket 0 is the grid cell CONTAINING the window start, never an earlier
// one. It must hold for every sub-day interval at every offset into the day.
func TestBucketStarts_FirstBucketContainsStart(t *testing.T) {
	loc := mustLondon(t)
	base := time.Date(2026, 6, 11, 0, 0, 0, 0, loc)

	for _, tok := range []string{"5m", "15m", "30m", "1h", "6h"} {
		iv := mustInterval(t, tok)
		// Sweep odd offsets into the day, including nanosecond grit.
		for _, off := range []time.Duration{
			0, time.Nanosecond, 61 * time.Second, 7 * time.Minute,
			37*time.Minute + 12*time.Second, 5*time.Hour + 3*time.Minute,
			11*time.Hour + 5*time.Minute + 453805045*time.Nanosecond,
			23*time.Hour + 59*time.Minute,
		} {
			start := base.Add(off)
			win := Window{Start: start, Stop: start.Add(3 * iv.Duration), Label: WindowCustom}
			got := BucketStarts(win, iv, loc)
			if len(got) == 0 {
				t.Fatalf("%s/+%s: no buckets", tok, off)
			}
			first := got[0]
			if first.After(start) {
				t.Errorf("%s/+%s: first bucket %s is after the window start %s", tok, off, first, start)
			}
			if !first.Add(iv.Duration).After(start) {
				t.Errorf("%s/+%s: first bucket %s is more than one interval before the start %s",
					tok, off, first, start)
			}
			if !onGrid(first, iv, loc) {
				t.Errorf("%s/+%s: first bucket %s is not on the local-midnight grid", tok, off, first)
			}
			// Every subsequent bucket is exactly one interval on.
			for i := 1; i < len(got); i++ {
				if !got[i].Equal(got[i-1].Add(iv.Duration)) {
					t.Errorf("%s/+%s: bucket %d = %s, want %s", tok, off, i, got[i], got[i-1].Add(iv.Duration))
				}
			}
		}
	}
}

// TestBucketStarts_AxisStableAcrossNow pins the second symptom in issue #18: the
// old skew was `now mod interval`, so two refreshes minutes apart rendered the
// same history at different offsets. On a snapped axis the labels are stable.
func TestBucketStarts_AxisStableAcrossNow(t *testing.T) {
	loc := mustLondon(t)
	iv := mustInterval(t, "1h")

	base := time.Date(2026, 8, 18, 11, 0, 0, 0, loc)
	var reference []time.Time
	for _, drift := range []time.Duration{
		0, 37 * time.Second, 5*time.Minute + 453805045*time.Nanosecond,
		29 * time.Minute, 59*time.Minute + 59*time.Second,
	} {
		now := base.Add(drift)
		win, err := ResolveWindow(now, loc, "24h", time.Time{}, time.Time{})
		if err != nil {
			t.Fatalf("ResolveWindow: %v", err)
		}
		got := BucketStarts(win, iv, loc)
		// Compare the shared history (drop the trailing partial bucket, which
		// legitimately appears once `now` crosses into the next cell).
		if reference == nil {
			reference = got
			continue
		}
		n := len(reference)
		if len(got) < n {
			n = len(got)
		}
		for i := 0; i < n; i++ {
			if !got[i].Equal(reference[i]) {
				t.Fatalf("drift %s: bucket %d = %s, want %s (axis must not move with now)",
					drift, i, got[i], reference[i])
			}
		}
	}
}

// TestBucketStarts_CalendarUnaffected guards that the 1d calendar branch, which
// was never broken, keeps stepping by local date.
func TestBucketStarts_CalendarUnaffected(t *testing.T) {
	loc := mustLondon(t)
	// Spans the autumn changeover (25h day) — the reason the branch exists.
	win := Window{
		Start: time.Date(2026, 10, 24, 9, 37, 0, 0, loc),
		Stop:  time.Date(2026, 10, 27, 3, 0, 0, 0, loc),
		Label: WindowCustom,
	}
	got := BucketStarts(win, mustInterval(t, "1d"), loc)
	want := []time.Time{
		time.Date(2026, 10, 24, 0, 0, 0, 0, loc),
		time.Date(2026, 10, 25, 0, 0, 0, 0, loc),
		time.Date(2026, 10, 26, 0, 0, 0, 0, loc),
		time.Date(2026, 10, 27, 0, 0, 0, 0, loc),
	}
	assertAxis(t, got, want)
}

// TestCountBuckets_AgreesWithOffGridAxis extends the cheap-counter contract to
// the off-grid windows the snap affects. countBuckets guards MaxBuckets without
// materialising the axis, so if it and BucketStarts disagree the cap is applied
// to a bucket count the response does not actually have.
func TestCountBuckets_AgreesWithOffGridAxis(t *testing.T) {
	loc := mustLondon(t)
	now := time.Date(2026, 8, 18, 11, 5, 0, 453805045, loc)

	type tc struct {
		name string
		win  Window
		tok  string
	}
	var cases []tc
	for _, spec := range []string{"24h", "48h", "6h", "1h"} {
		win, err := ResolveWindow(now, loc, spec, time.Time{}, time.Time{})
		if err != nil {
			t.Fatalf("ResolveWindow(%s): %v", spec, err)
		}
		for _, tok := range []string{"5m", "15m", "30m", "1h", "6h"} {
			cases = append(cases, tc{"window=" + spec + "/" + tok, win, tok})
		}
	}
	custom := Window{
		Start: time.Date(2026, 6, 11, 9, 37, 12, 0, loc),
		Stop:  time.Date(2026, 6, 12, 1, 2, 3, 0, loc),
		Label: WindowCustom,
	}
	for _, tok := range []string{"5m", "15m", "30m", "1h", "6h", "1d"} {
		cases = append(cases, tc{"custom/" + tok, custom, tok})
	}

	for _, c := range cases {
		iv := mustInterval(t, c.tok)
		want := len(BucketStarts(c.win, iv, loc))
		if got := countBuckets(c.win, iv, loc); got != want {
			t.Errorf("%s: countBuckets = %d, want len(BucketStarts) = %d", c.name, got, want)
		}
	}
}

// assertAxis compares a produced axis against an exact expectation.
func assertAxis(t *testing.T, got, want []time.Time) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("axis length = %d, want %d\n got: %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Errorf("bucket %d = %s, want %s", i, got[i], want[i])
		}
	}
}

// TestFixedIntervalsDivideTheDay pins the assumption fixedAxisStart rests on.
// BucketStarts snaps to the grid once and then steps by iv.Duration for the rest
// of the axis, while Flux re-anchors its location-aware grid at every local
// midnight. The two stay in agreement only while every fixed interval divides a
// 24h day evenly.
//
// An interval that did not would be on-grid for bucket 0 and off it after the
// first midnight in the window — a partial, tail-only recurrence of issue #18,
// and a quiet one: countBuckets would still agree with BucketStarts while both
// disagreed with Influx, so the axis-agreement contract in dos_test.go would not
// catch it either.
//
// Note the trap in picking a counter-example: 45m and 90m both DO divide 1440
// minutes (32 and 16 buckets), so neither would trip this. Genuinely unsafe
// additions look like 7m, 50m or 5h.
func TestFixedIntervalsDivideTheDay(t *testing.T) {
	for _, iv := range intervals {
		if iv.Calendar {
			continue // calendar days are stepped by date, not by duration
		}
		if 24*time.Hour%iv.Duration != 0 {
			t.Errorf("interval %q (%s) does not divide a 24h day evenly; the "+
				"local-midnight grid anchor in fixedAxisStart assumes it does, so "+
				"the axis would drift off Influx's grid after the first midnight",
				iv.Token, iv.Duration)
		}
	}
}
