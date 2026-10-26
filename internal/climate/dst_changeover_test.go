package climate

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/sweeney/greenhouse/internal/config"
	"github.com/sweeney/greenhouse/internal/influx"
)

// --- Issue #36: every interval across both clock changes ---------------------
//
// These run BuildSeries against a GaugeSim, which windows the readings the way
// the FLUX asks (and refuses a sub-day window on local time, the shape Influx
// stretches). Each bucket must hold fn over exactly the span it is labelled
// with — [its start, the next bucket's start) — on both changeover days, for
// every interval and every fn, for a single device and for a room mean.

// changeoverReadings is a report-on-change-shaped signal: the cadence varies by
// hour (every 1, 7 or 13 minutes), so an hour's sample count is not constant
// and a mean of hourly means is NOT the sample mean of a coarser bucket. The
// value wanders up and down so min, max and last are all distinct from mean.
func changeoverReadings(from, to time.Time, base float64) ([]time.Time, []float64) {
	var at []time.Time
	var v []float64
	cadence := []time.Duration{time.Minute, 7 * time.Minute, 13 * time.Minute}
	for t := from; t.Before(to); {
		m := t.Sub(from).Minutes()
		at = append(at, t)
		v = append(v, base+0.02*m+0.8*math.Sin(m/23))
		t = t.Add(cadence[(t.Unix()/3600)%3])
	}
	return at, v
}

// spanAggregate is fn over the readings in [lo, hi), NaN when there are none.
func spanAggregate(fn string, at []time.Time, v []float64, lo, hi time.Time) float64 {
	var in []float64
	for i, a := range at {
		if !a.Before(lo) && a.Before(hi) {
			in = append(in, v[i])
		}
	}
	if len(in) == 0 {
		return math.NaN()
	}
	switch fn {
	case "min":
		m := math.Inf(1)
		for _, x := range in {
			m = math.Min(m, x)
		}
		return m
	case "max":
		m := math.Inf(-1)
		for _, x := range in {
			m = math.Max(m, x)
		}
		return m
	case "last":
		return in[len(in)-1]
	default:
		s := 0.0
		for _, x := range in {
			s += x
		}
		return s / float64(len(in))
	}
}

// bucketSpans returns each bucket's real span, clipped to the window: the first
// starts no earlier than win.Start (the query range does), the last ends at
// win.Stop, and every other one ends where the next begins.
func bucketSpans(buckets []time.Time, win Window) [][2]time.Time {
	out := make([][2]time.Time, len(buckets))
	for i, b := range buckets {
		lo, hi := b, win.Stop
		if i+1 < len(buckets) {
			hi = buckets[i+1]
		}
		if lo.Before(win.Start) {
			lo = win.Start
		}
		out[i] = [2]time.Time{lo, hi}
	}
	return out
}

type changeoverCase struct {
	name string
	win  Window
}

func changeoverWindows(t *testing.T) []changeoverCase {
	t.Helper()
	loc := mustLondon(t)
	rolling := func(now time.Time, spec string) Window {
		w, err := ResolveWindow(now, loc, spec, time.Time{}, time.Time{})
		if err != nil {
			t.Fatalf("ResolveWindow(%s): %v", spec, err)
		}
		return w
	}
	return []changeoverCase{
		{"autumn/custom", Window{Start: time.Date(2026, 10, 24, 18, 0, 0, 0, loc), Stop: time.Date(2026, 10, 26, 6, 0, 0, 0, loc), Label: WindowCustom}},
		{"spring/custom", Window{Start: time.Date(2026, 3, 28, 18, 0, 0, 0, loc), Stop: time.Date(2026, 3, 30, 6, 0, 0, 0, loc), Label: WindowCustom}},
		// The issue's window shape: a rolling 48h, off every grid.
		{"autumn/48h", rolling(time.Date(2026, 10, 26, 8, 7, 12, 345678901, loc), "48h")},
		{"spring/48h", rolling(time.Date(2026, 3, 30, 8, 7, 12, 345678901, loc), "48h")},
	}
}

// TestBuildSeries_EveryBucketCarriesItsOwnSpanAcrossClockChanges is the sweep:
// both London changes × every interval × every fn × device and room grouping.
func TestBuildSeries_EveryBucketCarriesItsOwnSpanAcrossClockChanges(t *testing.T) {
	loc := mustLondon(t)
	devices := map[string]config.DeviceConfig{
		"sensor_a": {Class: "environmental_sensor", DisplayName: "Sensor A", Room: "r1"},
		"sensor_b": {Class: "environmental_sensor", DisplayName: "Sensor B", Room: "r1"},
	}
	for _, c := range changeoverWindows(t) {
		from, to := c.win.Start.Add(-2*time.Hour), c.win.Stop.Add(2*time.Hour)
		atA, vA := changeoverReadings(from, to, 4)
		atB, vB := changeoverReadings(from.Add(3*time.Minute), to, 9)
		sim := influx.NewGaugeSim("temperature_c").AddSamples("sensor_a", atA, vA).AddSamples("sensor_b", atB, vB)

		for _, tok := range AllowedIntervals() {
			iv := mustInterval(t, tok)
			if countBuckets(c.win, iv, loc) > MaxBuckets {
				continue
			}
			for _, fn := range []string{"mean", "min", "max", "last"} {
				t.Run(fmt.Sprintf("%s/%s/%s", c.name, tok, fn), func(t *testing.T) {
					byDevice, err := BuildSeries(context.Background(), sim, "statehouse", c.win, iv,
						"temperature_c", fn, GroupByDevice, DefaultGroupFn, devices, nil, loc)
					if err != nil {
						t.Fatalf("BuildSeries device: %v", err)
					}
					byRoom, err := BuildSeries(context.Background(), sim, "statehouse", c.win, iv,
						"temperature_c", fn, GroupByRoom, GroupFnMean, devices, nil, loc)
					if err != nil {
						t.Fatalf("BuildSeries room: %v", err)
					}
					spans := bucketSpans(byDevice.Buckets, c.win)
					wantA := make([]float64, len(spans))
					wantB := make([]float64, len(spans))
					for i, s := range spans {
						wantA[i] = spanAggregate(fn, atA, vA, s[0], s[1])
						wantB[i] = spanAggregate(fn, atB, vB, s[0], s[1])
					}
					// The room mean is over the members that reported in the bucket.
					wantRoom := make([]float64, len(spans))
					for i := range spans {
						switch a, b := wantA[i], wantB[i]; {
						case math.IsNaN(a):
							wantRoom[i] = b
						case math.IsNaN(b):
							wantRoom[i] = a
						default:
							wantRoom[i] = (a + b) / 2
						}
					}
					assertSpanValues(t, "sensor_a", byDevice.Buckets, seriesValues(t, byDevice, "sensor_a"), wantA)
					assertSpanValues(t, "sensor_b", byDevice.Buckets, seriesValues(t, byDevice, "sensor_b"), wantB)
					assertSpanValues(t, "room r1", byRoom.Buckets, seriesValues(t, byRoom, "r1"), wantRoom)
				})
			}
		}
	}
}

func seriesValues(t *testing.T, resp SeriesResponse, key string) []float64 {
	t.Helper()
	for _, s := range resp.Series {
		if s.Key == key {
			return s.Values
		}
	}
	t.Fatalf("no series %q in response", key)
	return nil
}

// assertSpanValues compares rounded response values with the exact aggregate of
// each bucket's span; anything beyond rounding (0.005) is the wrong span.
func assertSpanValues(t *testing.T, label string, buckets []time.Time, got, want []float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d values for %d buckets", label, len(got), len(want))
	}
	bad := 0
	for i := range want {
		b := buckets[i].Format(time.RFC3339)
		switch {
		case math.IsNaN(want[i]) && !math.IsNaN(got[i]):
			t.Errorf("%s: bucket %s = %v, want a gap", label, b, got[i])
		case !math.IsNaN(want[i]) && math.IsNaN(got[i]):
			t.Errorf("%s: bucket %s is null, want %.4f", label, b, want[i])
		case !math.IsNaN(want[i]) && math.Abs(got[i]-want[i]) > 0.005+1e-9:
			t.Errorf("%s: bucket %s = %v, want %.4f (fn over the span it is labelled with)", label, b, got[i], want[i])
		default:
			continue
		}
		if bad++; bad >= 5 {
			t.Fatalf("%s: stopping after %d bad buckets", label, bad)
		}
	}
}

// TestBucketStarts_6hFollowsLocalWallClock: a 6h axis is 00/06/12/18 LOCAL on
// both sides of a change, so the bucket straddling it is 7h (autumn) or 5h
// (spring). A fixed 6h step drifted an hour off that grid after the change
// (autumn: 00:00+01:00, 05:00Z, 11:00Z, 17:00Z).
func TestBucketStarts_6hFollowsLocalWallClock(t *testing.T) {
	loc := mustLondon(t)
	iv := mustInterval(t, "6h")
	for _, c := range []struct {
		name string
		day  time.Time
		want []string
	}{
		{"autumn", time.Date(2026, 10, 25, 0, 0, 0, 0, loc),
			[]string{"2026-10-25T00:00:00+01:00", "2026-10-25T06:00:00Z", "2026-10-25T12:00:00Z", "2026-10-25T18:00:00Z"}},
		{"spring", time.Date(2026, 3, 29, 0, 0, 0, 0, loc),
			[]string{"2026-03-29T00:00:00Z", "2026-03-29T06:00:00+01:00", "2026-03-29T12:00:00+01:00", "2026-03-29T18:00:00+01:00"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			win := Window{Start: c.day, Stop: c.day.AddDate(0, 0, 1), Label: WindowCustom}
			got := BucketStarts(win, iv, loc)
			if len(got) != len(c.want) {
				t.Fatalf("got %d buckets %v, want %v", len(got), got, c.want)
			}
			for i := range got {
				if s := got[i].Format(time.RFC3339); s != c.want[i] {
					t.Errorf("bucket %d = %s, want %s", i, s, c.want[i])
				}
			}
		})
	}

	// Over several days either side, every boundary stays on the local grid.
	for _, c := range changeoverWindows(t) {
		for i, b := range BucketStarts(c.win, iv, loc) {
			lb := b.In(loc)
			if lb.Hour()%6 != 0 || lb.Minute() != 0 || lb.Second() != 0 || lb.Nanosecond() != 0 {
				t.Errorf("%s: bucket %d = %s is off the local 00/06/12/18 grid", c.name, i, b.Format(time.RFC3339))
			}
		}
	}
}

// TestBucketStarts_SubDayIsTheAbsoluteGrid: at 1h and finer the axis is a fixed
// step on the absolute grid, the grid the query windows on. The autumn day
// therefore has 25 hourly buckets (both 01:00s) and spring 23.
func TestBucketStarts_SubDayIsTheAbsoluteGrid(t *testing.T) {
	loc := mustLondon(t)
	for _, c := range []struct {
		day         time.Time
		hours       int
		autumnLabel string
	}{
		{time.Date(2026, 10, 25, 0, 0, 0, 0, loc), 25, "autumn"},
		{time.Date(2026, 3, 29, 0, 0, 0, 0, loc), 23, "spring"},
	} {
		win := Window{Start: c.day, Stop: c.day.AddDate(0, 0, 1), Label: WindowCustom}
		for _, tok := range []string{"5m", "15m", "30m", "1h"} {
			iv := mustInterval(t, tok)
			got := BucketStarts(win, iv, loc)
			if want := c.hours * int(time.Hour/iv.Duration); len(got) != want {
				t.Errorf("%s/%s: %d buckets, want %d", c.autumnLabel, tok, len(got), want)
			}
			for i, b := range got {
				if !b.Equal(b.Truncate(iv.Duration)) {
					t.Errorf("%s/%s: bucket %d = %s is off the absolute grid", c.autumnLabel, tok, i, b)
				}
				if i > 0 && b.Sub(got[i-1]) != iv.Duration {
					t.Errorf("%s/%s: bucket %d is %s after the previous, want %s", c.autumnLabel, tok, i, b.Sub(got[i-1]), iv.Duration)
				}
			}
		}
	}
}

// TestCountBuckets_AgreesAcrossClockChanges extends the cheap-counter contract
// to the changeover windows, for every interval.
func TestCountBuckets_AgreesAcrossClockChanges(t *testing.T) {
	loc := mustLondon(t)
	for _, c := range changeoverWindows(t) {
		for _, tok := range AllowedIntervals() {
			iv := mustInterval(t, tok)
			if got, want := countBuckets(c.win, iv, loc), len(BucketStarts(c.win, iv, loc)); got != want {
				t.Errorf("%s/%s: countBuckets = %d, want len(BucketStarts) = %d", c.name, tok, got, want)
			}
		}
	}
}

// withDeadline fails the test instead of hanging it when fn does not return.
func withDeadline(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not return within 5s (an axis loop that never ends hangs /series)", what)
	}
}

// TestBucketStarts_SkippedMidnight covers zones whose clocks go forward AT
// local midnight (America/Santiago, America/Havana): that day's 00:00 does not
// exist. Each 1d/6h boundary must be resolved on its own — the skipped one
// starts at the transition instant — instead of being stepped from the
// previous resolved instant, which left the 6h axis on 05/11/17/23 for good
// and made the 1d loop return the same midnight forever (PR countinghouse#50
// review). Config accepts these zones: their offsets are whole hours.
func TestBucketStarts_SkippedMidnight(t *testing.T) {
	for _, c := range []struct {
		zone string
		day  [3]int
	}{
		{"America/Santiago", [3]int{2026, 9, 6}},
		{"America/Havana", [3]int{2026, 3, 8}},
	} {
		loc, err := time.LoadLocation(c.zone)
		if err != nil {
			t.Fatalf("load %s: %v", c.zone, err)
		}
		day := time.Date(c.day[0], time.Month(c.day[1]), c.day[2], 12, 0, 0, 0, loc)
		if _, off := day.Zone(); func() bool { _, o := day.AddDate(0, 0, -1).Zone(); return o == off }() {
			t.Fatalf("%s: no clock change on %v; fixture date is stale", c.zone, c.day)
		}
		win := Window{Start: day.AddDate(0, 0, -3), Stop: day.AddDate(0, 0, 3), Label: WindowCustom}
		for _, tok := range []string{"6h", "1d"} {
			iv := mustInterval(t, tok)
			var got []time.Time
			var n int
			withDeadline(t, c.zone+"/"+tok+" BucketStarts", func() { got = BucketStarts(win, iv, loc) })
			withDeadline(t, c.zone+"/"+tok+" countBuckets", func() { n = countBuckets(win, iv, loc) })
			if n != len(got) {
				t.Errorf("%s/%s: countBuckets = %d, len(BucketStarts) = %d", c.zone, tok, n, len(got))
			}
			step := 6
			if iv.Calendar {
				step = 24
			}
			for i, b := range got {
				if i > 0 && !b.After(got[i-1]) {
					t.Errorf("%s/%s: bucket %d (%s) is not after %s", c.zone, tok, i, b, got[i-1])
				}
				lb := b.In(loc)
				onGrid := lb.Hour()%step == 0 && lb.Minute() == 0 && lb.Second() == 0
				zs, _ := b.ZoneBounds()
				if !onGrid && !b.Equal(zs) {
					t.Errorf("%s/%s: bucket %d = %s is neither on the local grid nor a clock change", c.zone, tok, i, lb)
				}
			}
			if want := 6 * 24 / step; len(got) < want-1 || len(got) > want+1 {
				t.Errorf("%s/%s: %d buckets over six days, want about %d: %v", c.zone, tok, len(got), want, got)
			}
		}
	}
}
