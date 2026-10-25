package climate

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/sweeney/greenhouse/internal/config"
	"github.com/sweeney/greenhouse/internal/influx"
)

// --- Influx replay harness for issue #18 -------------------------------------
//
// The unit tests above pin the axis. These prove the behaviour the user sees, by
// replaying what InfluxDB actually returns for the query greenhouse builds and
// asserting each bucket's value belongs to the span its label claims.
//
// The harness is an influx.GaugeSim, which answers the flux BuildSeries actually
// sends, windowing the readings on the grid that flux asks for, in the two
// respects that caused the bug:
//
//  1. Windows sit on the GRID, not on the query's range start. So a range
//     starting 11:05 still yields boundaries at 11:00, 12:00, 13:00, …
//  2. The first window is TRUNCATED to range start, so its `_start` stamp is the
//     range start itself, not the grid boundary. That extra row is why 25 rows
//     used to arrive for a 24-slot axis.
//
// It used to be a hand-rolled replay that ignored the flux and stepped a fixed
// duration from local midnight — the very grid the Go axis assumed — so it
// could not see issue #36, where the query's grid and the axis disagreed only
// across a clock change. Reading the grid from the flux is what lets it.

// sample is one underlying sensor reading.
type sample struct {
	At time.Time
	V  float64
}

// rampSamples builds a per-minute signal over [start, stop) that is strictly
// increasing, so adjacent buckets never share a value. The issue notes that ties
// masked the bug in real data ("ambiguous (values equal): 16") — a strictly
// monotone signal removes that cover entirely.
func rampSamples(start, stop time.Time) []sample {
	var out []sample
	for i, t := 0, start; t.Before(stop); i, t = i+1, t.Add(time.Minute) {
		out = append(out, sample{At: t, V: 10 + 0.37*float64(i)})
	}
	return out
}

// meanOver returns the mean of the samples in [lo, hi), or NaN if none.
func meanOver(samples []sample, lo, hi time.Time) float64 {
	sum, n := 0.0, 0
	for _, s := range samples {
		if !s.At.Before(lo) && s.At.Before(hi) {
			sum += s.V
			n++
		}
	}
	if n == 0 {
		return math.NaN()
	}
	return sum / float64(n)
}

// gaugeSimOf loads samples into a GaugeSim for one device's temperature_c.
func gaugeSimOf(deviceID string, samples []sample) *influx.GaugeSim {
	at := make([]time.Time, len(samples))
	v := make([]float64, len(samples))
	for i, s := range samples {
		at[i], v[i] = s.At, s.V
	}
	return influx.NewGaugeSim("temperature_c").AddSamples(deviceID, at, v)
}

// replayAggregateWindow returns the rows Influx produces for the mean field
// series greenhouse builds over win at iv.
func replayAggregateWindow(t *testing.T, deviceID string, samples []sample, win Window, iv Interval, loc *time.Location) []influx.Row {
	t.Helper()
	flux := influx.BuildFieldSeriesFlux("statehouse", []string{deviceID}, "temperature_c", "mean", win.Start, win.Stop, iv.Token, loc.String())
	rows, err := gaugeSimOf(deviceID, samples).Answer(flux)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	return rows
}

// assertBucketsCarryTheirOwnSpan is the core assertion of issue #18: for every
// bucket on the canonical axis, the reported value must equal the aggregate of
// the time span that bucket is LABELLED with (clipped to the window), not the
// span of a neighbouring bucket.
func assertBucketsCarryTheirOwnSpan(t *testing.T, label string, resp SeriesResponse, samples []sample, win Window, iv Interval) {
	t.Helper()
	if len(resp.Series) != 1 {
		t.Fatalf("%s: want 1 series, got %d", label, len(resp.Series))
	}
	got := resp.Series[0].Values
	if len(got) != len(resp.Buckets) {
		t.Fatalf("%s: %d values for %d buckets", label, len(got), len(resp.Buckets))
	}

	// The query's range() bounds are rendered to whole seconds (influx.fluxTime),
	// so a reading in the sub-second slice before a nanosecond-precision "now"
	// is outside the range. The GaugeSim honours the rendered bounds, as Influx
	// does, so the expectation clips to them too.
	start, stop := win.Start.Truncate(time.Second), win.Stop.Truncate(time.Second)

	shifted := 0
	for i, b := range resp.Buckets {
		lo, hi := b, b.Add(iv.Duration)
		if lo.Before(start) {
			lo = start
		}
		if hi.After(stop) {
			hi = stop
		}
		exact := meanOver(samples, lo, hi)
		want := roundTo(exact, valueDP)

		if math.IsNaN(want) {
			if !math.IsNaN(got[i]) {
				t.Errorf("%s: bucket %d (%s) = %v, want a gap", label, i, b, got[i])
			}
			continue
		}
		if math.IsNaN(got[i]) {
			t.Errorf("%s: bucket %d (%s) is a gap, want %v", label, i, b, want)
			continue
		}
		// Against the exact mean, within rounding: a 6h mean is folded from
		// hourly sums and counts (issue #36), which can differ from a direct
		// mean in the last bit and so round the other way at a .xx5 boundary.
		if math.Abs(got[i]-exact) > 0.005+1e-9 {
			// Name the failure mode explicitly when the value is the NEXT
			// bucket's — that is the exact signature reported in issue #18.
			next := roundTo(meanOver(samples, b.Add(iv.Duration), b.Add(2*iv.Duration)), valueDP)
			if !math.IsNaN(next) && math.Abs(got[i]-next) < 1e-9 {
				shifted++
				t.Errorf("%s: bucket %d (%s) = %v — that is the NEXT bucket's value; want %v (series shifted one bucket early)",
					label, i, b, got[i], want)
			} else {
				t.Errorf("%s: bucket %d (%s) = %v, want %v", label, i, b, got[i], want)
			}
		}
	}
	if shifted > 0 {
		t.Errorf("%s: %d/%d buckets carried the following bucket's value", label, shifted, len(got))
	}
}

// buildReplayed runs BuildSeries against a fake Influx that replays the real
// aggregateWindow behaviour for the window/interval under test.
func buildReplayed(t *testing.T, win Window, iv Interval, loc *time.Location) (SeriesResponse, []sample) {
	t.Helper()
	const dev = "sensor_a"
	devices := map[string]config.DeviceConfig{
		dev: {Class: "environmental_sensor", Location: "area-a", DisplayName: "Sensor A"},
	}
	samples := rampSamples(win.Start, win.Stop)
	q := &influx.FakeQuerier{PingOK: true, QueryFunc: gaugeSimOf(dev, samples).Answer}
	resp, err := BuildSeries(context.Background(), q, "statehouse", win, iv, "temperature_c", "mean", GroupByDevice, DefaultGroupFn, devices, nil, loc)
	if err != nil {
		t.Fatalf("BuildSeries: %v", err)
	}
	return resp, samples
}

// TestBuildSeries_OffGridWindowsCarryTheirOwnSpan is the end-to-end reproduction
// of issue #18 across every affected window form and sub-day interval.
func TestBuildSeries_OffGridWindowsCarryTheirOwnSpan(t *testing.T) {
	loc := mustLondon(t)
	// Nanosecond-precision "now", exactly as the injected clock supplies it.
	now := time.Date(2026, 8, 18, 11, 5, 0, 453805045, loc)

	rolling := func(spec string) Window {
		w, err := ResolveWindow(now, loc, spec, time.Time{}, time.Time{})
		if err != nil {
			t.Fatalf("ResolveWindow(%s): %v", spec, err)
		}
		return w
	}

	type namedWindow struct {
		name string
		win  Window
	}
	windows := []namedWindow{
		{"window=24h", rolling("24h")},
		{"window=48h", rolling("48h")},
		{"window=6h", rolling("6h")},
		{"window=custom off-grid from", Window{
			Start: time.Date(2026, 6, 11, 9, 37, 12, 0, loc),
			Stop:  time.Date(2026, 6, 12, 2, 13, 44, 0, loc),
			Label: WindowCustom,
		}},
	}

	for _, w := range windows {
		for _, tok := range []string{"5m", "15m", "30m", "1h", "6h"} {
			iv := mustInterval(t, tok)
			if countBuckets(w.win, iv, loc) > MaxBuckets {
				continue
			}
			t.Run(w.name+"/"+tok, func(t *testing.T) {
				resp, samples := buildReplayed(t, w.win, iv, loc)
				assertBucketsCarryTheirOwnSpan(t, w.name+"/"+tok, resp, samples, w.win, iv)
			})
		}
	}
}

// TestBuildSeries_OnGridWindowsStayCorrect is the control. The issue's evidence
// used window=today as the known-good reference; these must not regress.
func TestBuildSeries_OnGridWindowsStayCorrect(t *testing.T) {
	loc := mustLondon(t)
	now := time.Date(2026, 8, 18, 11, 5, 0, 453805045, loc)

	for _, spec := range []string{WindowToday, "2d"} {
		win, err := ResolveWindow(now, loc, spec, time.Time{}, time.Time{})
		if err != nil {
			t.Fatalf("ResolveWindow(%s): %v", spec, err)
		}
		for _, tok := range []string{"15m", "1h", "6h"} {
			iv := mustInterval(t, tok)
			t.Run(spec+"/"+tok, func(t *testing.T) {
				resp, samples := buildReplayed(t, win, iv, loc)
				assertBucketsCarryTheirOwnSpan(t, spec+"/"+tok, resp, samples, win, iv)
			})
		}
	}
}

// TestBuildSeries_OffGridFirstPartialWindowSurvives pins the second consequence
// in issue #18: Flux truncates its first window to range start, so an off-grid
// 24h/1h query returns 25 rows. Against an unsnapped 24-slot axis the second row
// snapped back on top of the first and that first partial reading was lost.
// Every returned row must now land in a DISTINCT bucket.
func TestBuildSeries_OffGridFirstPartialWindowSurvives(t *testing.T) {
	loc := mustLondon(t)
	now := time.Date(2026, 8, 18, 11, 5, 0, 453805045, loc)
	win, err := ResolveWindow(now, loc, "24h", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("ResolveWindow: %v", err)
	}
	iv := mustInterval(t, "1h")

	samples := rampSamples(win.Start, win.Stop)
	rows := replayAggregateWindow(t, "d", samples, win, iv, loc)
	if len(rows) != 25 {
		t.Fatalf("replay produced %d rows, want 25 (24 full hours plus the truncated first window)", len(rows))
	}

	buckets := BucketStarts(win, iv, loc)
	idx := bucketIndex(buckets)
	dst := map[string][]float64{}
	demux(rows, idx, dst, len(buckets))

	// No two rows may resolve to the same slot: that collision is the data loss.
	starts := make([]int64, 0, len(idx))
	for k := range idx {
		starts = append(starts, k)
	}
	sortInt64s(starts)
	seen := map[int]time.Time{}
	for _, r := range rows {
		i := resolveBucket(r.Time, idx, starts)
		if i < 0 {
			t.Errorf("row at %s resolved outside the axis", r.Time)
			continue
		}
		if prev, dup := seen[i]; dup {
			t.Errorf("rows at %s and %s both resolved to bucket %d (%s) — the earlier value is overwritten",
				prev, r.Time, i, buckets[i])
			continue
		}
		seen[i] = r.Time
	}

	// The truncated first row must survive as bucket 0's value.
	first := roundTo(meanOver(samples, win.Start, buckets[0].Add(iv.Duration)), valueDP)
	if got := roundTo(dst["d"][0], valueDP); math.Abs(got-first) > 1e-9 {
		t.Errorf("bucket 0 = %v, want %v (the truncated first window's own value)", got, first)
	}
}

// TestBuildSeries_HeatingRampLandsInTheRightHour is issue #18 read back as the
// symptom the operator actually noticed: on the skewed axis this device's
// morning heating ramp was reported as starting an hour before it happened,
// which made temperature appear to respond BEFORE the heating that caused it in
// the countinghouse overlay.
func TestBuildSeries_HeatingRampLandsInTheRightHour(t *testing.T) {
	loc := mustLondon(t)
	now := time.Date(2026, 8, 18, 11, 5, 0, 453805045, loc)
	win, err := ResolveWindow(now, loc, "24h", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("ResolveWindow: %v", err)
	}
	iv := mustInterval(t, "1h")

	// Flat at 21.6 °C until 07:00, then a sharp ramp to 23.94 °C during 07:00-08:00.
	rampStart := time.Date(2026, 8, 18, 7, 0, 0, 0, loc)
	rampEnd := rampStart.Add(time.Hour)
	var samples []sample
	for tm := win.Start; tm.Before(win.Stop); tm = tm.Add(time.Minute) {
		v := 21.6
		if !tm.Before(rampEnd) {
			v = 26.0
		} else if !tm.Before(rampStart) {
			v = 23.94
		}
		samples = append(samples, sample{At: tm, V: v})
	}

	const dev = "sensor_a"
	devices := map[string]config.DeviceConfig{dev: {Class: "environmental_sensor", DisplayName: "Sensor A"}}
	q := &influx.FakeQuerier{PingOK: true, QueryFunc: gaugeSimOf(dev, samples).Answer}
	resp, err := BuildSeries(context.Background(), q, "statehouse", win, iv, "temperature_c", "mean", GroupByDevice, DefaultGroupFn, devices, nil, loc)
	if err != nil {
		t.Fatalf("BuildSeries: %v", err)
	}

	at := func(h int) (time.Time, float64) {
		want := time.Date(2026, 8, 18, h, 0, 0, 0, loc)
		for i, b := range resp.Buckets {
			if b.Equal(want) {
				return b, resp.Series[0].Values[i]
			}
		}
		t.Fatalf("no bucket labelled %s in %v", want, resp.Buckets)
		return time.Time{}, 0
	}

	if _, v := at(6); math.Abs(v-21.6) > 1e-9 {
		t.Errorf("06:00 bucket = %v, want 21.6 (the ramp had not started yet)", v)
	}
	if _, v := at(7); math.Abs(v-23.94) > 1e-9 {
		t.Errorf("07:00 bucket = %v, want 23.94 (the ramp happened between 07:00 and 08:00)", v)
	}
	if _, v := at(8); math.Abs(v-26.0) > 1e-9 {
		t.Errorf("08:00 bucket = %v, want 26", v)
	}
}

// sortInt64s is a tiny local sort so the replay tests need no extra imports.
func sortInt64s(x []int64) {
	for i := 1; i < len(x); i++ {
		for j := i; j > 0 && x[j] < x[j-1]; j-- {
			x[j], x[j-1] = x[j-1], x[j]
		}
	}
}
