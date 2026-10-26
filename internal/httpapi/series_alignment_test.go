package httpapi

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sweeney/greenhouse/internal/climate"
	"github.com/sweeney/greenhouse/internal/config"
	"github.com/sweeney/greenhouse/internal/influx"
	"github.com/sweeney/greenhouse/internal/testutil"
)

// --- HTTP-level regression suite for issue #18 -------------------------------
//
// The climate package proves the axis and the demux. These drive the real
// handler over the real router and assert on the JSON a consumer receives,
// because that is where the bug was observed: both greenhouse demo pages default
// to window=24h, so the default view was the skewed one.

// offGridNow is the instant from the issue's prod evidence: 2026-08-18 11:05 BST
// with the nanosecond grit a real injected clock carries. Every "<N>h" window
// derived from it starts 5 minutes and change off the interval grid.
func offGridNow(t *testing.T, loc *time.Location) time.Time {
	t.Helper()
	return time.Date(2026, 8, 18, 11, 5, 0, 453805045, loc)
}

// alignedSeriesResp decodes the columnar /series payload with the bucket axis as
// raw strings, so the test asserts on the exact RFC3339 text sent to consumers
// rather than on a re-parsed time.Time.
type alignedSeriesResp struct {
	Window   string   `json:"window"`
	From     string   `json:"from"`
	To       string   `json:"to"`
	Interval string   `json:"interval"`
	Field    string   `json:"field"`
	Buckets  []string `json:"buckets"`
	Series   []struct {
		Key    string     `json:"key"`
		Values []*float64 `json:"values"`
	} `json:"series"`
}

// rampingReplay wires the fake querier to return what Influx really returns for
// the window under test: aggregateWindow rows on the grid the FLUX asks for,
// stamped with timeSrc:"_start", with the first window truncated to the range
// start. Readings ramp by 1 °C per hour of real time from 10 °C at the window
// start, so no two hourly buckets share a value.
func rampingReplay(s *Server, deviceID, spec string) func(string) ([]influx.Row, error) {
	return func(flux string) ([]influx.Row, error) {
		win, err := climate.ResolveWindow(s.Clock.Now(), s.Loc, spec, time.Time{}, time.Time{})
		if err != nil {
			return nil, err
		}
		return replayGridRows(deviceID, win, flux)
	}
}

var (
	replayEveryRe  = regexp.MustCompile(`every: (\w+),`)
	replayFnRe     = regexp.MustCompile(`aggregateWindow\(every: \w+, fn: (\w+),`)
	replayUTCRe    = regexp.MustCompile(`location: timezone\.utc[,)]`)
	replaySumCount = `|> window(every: 1h, location: timezone.utc)
  |> reduce(identity: {sum: 0.0, count: 0.0}, fn: (r, accumulator) => ({sum: accumulator.sum + float(v: r._value), count: accumulator.count + 1.0}))
  |> duplicate(column: "_start", as: "_time")`
)

// replayGridRows produces the rows for [win.Start, win.Stop) at the flux's
// every: on the UTC grid, for the two shapes greenhouse sends: a mean
// aggregateWindow, and the single-pass window|>reduce sum+count the 6h mean is
// folded from (issue #36, PR #37 review). The ramp is modelled as continuous: a
// window's mean is the temperature at its midpoint, its count is its length in
// minutes (one reading a minute) and its sum is mean × count. That makes the
// hourly sum/count fold back to exactly the 6h window's midpoint, as it would
// for a real linear ramp. Any other shape is refused, so a builder change
// cannot be answered with what the caller meant rather than what it sent.
//
// It used to step the interval token from local midnight whatever the flux
// asked for, so it answered a 1h fold query with 6h rows; it now refuses a
// sub-day window on local time, the shape Influx stretches across a clock
// change (see influx.GaugeSim).
func replayGridRows(deviceID string, win climate.Window, flux string) ([]influx.Row, error) {
	em := replayEveryRe.FindStringSubmatch(flux)
	if em == nil {
		return nil, fmt.Errorf("replay: no every: in flux:\n%s", flux)
	}
	step, err := time.ParseDuration(em[1])
	if err != nil {
		return nil, err
	}
	if !replayUTCRe.MatchString(flux) {
		return nil, fmt.Errorf("replay: only sub-day windows on timezone.utc are modelled:\n%s", flux)
	}
	sumCount := strings.Contains(flux, replaySumCount)
	if fm := replayFnRe.FindStringSubmatch(flux); !sumCount && (fm == nil || fm[1] != "mean") {
		return nil, fmt.Errorf("replay: only a mean aggregateWindow or the sum/count reduce is modelled:\n%s", flux)
	}

	var rows []influx.Row
	for edge := win.Start.Truncate(step); edge.Before(win.Stop); edge = edge.Add(step) {
		lo, hi := edge, edge.Add(step)
		if lo.Before(win.Start) {
			lo = win.Start // Flux truncates the first window to range start
		}
		if hi.After(win.Stop) {
			hi = win.Stop
		}
		mid := lo.Add(hi.Sub(lo) / 2)
		mean := 10 + mid.Sub(win.Start).Hours()
		row := influx.Row{DeviceID: deviceID, Time: lo} // left edge
		if sumCount {
			minutes := hi.Sub(lo).Minutes()
			row.Sum, row.Count, row.HasSumCount = mean*minutes, minutes, true
		} else {
			row.Value, row.HasValue = mean, true
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// alignedSetup returns a Server whose clock sits at the off-grid instant, with a
// single environmental device so the series is unambiguous.
func alignedSetup(t *testing.T) (*Server, *influx.FakeQuerier) {
	t.Helper()
	s, q := dataSetup(t)
	s.Clock = testutil.NewFakeClock(offGridNow(t, s.Loc))
	s.Config = fakeConfig{devices: map[string]config.DeviceConfig{
		"sensor_a": {Class: "environmental_sensor", Location: "area-a", DisplayName: "Sensor A"},
	}}
	return s, q
}

// TestSeriesHTTP_RollingWindowBucketsAreOnTheGrid is the consumer-visible half of
// issue #18: the default demo view (window=24h) must publish whole-hour bucket
// labels, not nanosecond-precision ones derived from `now`.
func TestSeriesHTTP_RollingWindowBucketsAreOnTheGrid(t *testing.T) {
	for _, tc := range []struct {
		interval string
		wantMod  time.Duration
		wantHead string
	}{
		{"15m", 15 * time.Minute, "2026-08-17T11:00:00+01:00"},
		{"1h", time.Hour, "2026-08-17T11:00:00+01:00"},
		{"6h", 6 * time.Hour, "2026-08-17T06:00:00+01:00"},
	} {
		t.Run(tc.interval, func(t *testing.T) {
			s, q := alignedSetup(t)
			q.QueryFunc = rampingReplay(s, "sensor_a", "24h")

			w := doGET(t, s, "/series?field=temperature_c&fn=mean&group_by=device&window=24h&interval="+tc.interval)
			if w.Code != http.StatusOK {
				t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
			}
			var resp alignedSeriesResp
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(resp.Buckets) == 0 {
				t.Fatal("no buckets")
			}
			if resp.Buckets[0] != tc.wantHead {
				t.Errorf("first bucket = %q, want %q", resp.Buckets[0], tc.wantHead)
			}
			for i, b := range resp.Buckets {
				ts, err := time.Parse(time.RFC3339Nano, b)
				if err != nil {
					t.Fatalf("bucket %d = %q: %v", i, b, err)
				}
				lt := ts.In(s.Loc)
				midnight := time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, s.Loc)
				if lt.Sub(midnight)%tc.wantMod != 0 {
					t.Errorf("bucket %d = %q is not on the %s grid", i, b, tc.interval)
				}
			}
		})
	}
}

// TestSeriesHTTP_RollingWindowValuesMatchTheirLabel is the substantive assertion:
// every published value must be the aggregate of the span its bucket is labelled
// with. Before the fix each value was the FOLLOWING span's, so a 1 °C-per-hour
// ramp read one degree high in every bucket.
func TestSeriesHTTP_RollingWindowValuesMatchTheirLabel(t *testing.T) {
	// 6h is folded from hourly sum and count (issue #36); its values must still
	// describe the span each bucket is labelled with.
	for _, tc := range []struct {
		interval string
		step     time.Duration
	}{{"1h", time.Hour}, {"6h", 6 * time.Hour}} {
		t.Run(tc.interval, func(t *testing.T) {
			s, q := alignedSetup(t)
			q.QueryFunc = rampingReplay(s, "sensor_a", "24h")

			win, err := climate.ResolveWindow(s.Clock.Now(), s.Loc, "24h", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("ResolveWindow: %v", err)
			}

			w := doGET(t, s, "/series?field=temperature_c&fn=mean&group_by=device&window=24h&interval="+tc.interval)
			if w.Code != http.StatusOK {
				t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
			}
			var resp alignedSeriesResp
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(resp.Series) != 1 {
				t.Fatalf("want 1 series, got %d", len(resp.Series))
			}

			for i, b := range resp.Buckets {
				ts, err := time.Parse(time.RFC3339Nano, b)
				if err != nil {
					t.Fatalf("bucket %d: %v", i, err)
				}
				lo, hi := ts, ts.Add(tc.step)
				if lo.Before(win.Start) {
					lo = win.Start
				}
				if hi.After(win.Stop) {
					hi = win.Stop
				}
				mid := lo.Add(hi.Sub(lo) / 2)
				exact := 10 + mid.Sub(win.Start).Hours()

				got := resp.Series[0].Values[i]
				if got == nil {
					t.Errorf("bucket %d (%s) is null, want %v", i, b, climate.RoundValue(exact))
					continue
				}
				// Strict where nothing is folded; within rounding for the 6h
				// fold, whose Σsum/Σcount can differ from a direct mean in the
				// last bit and round the other way at a .xx5 boundary.
				diff, tol := math.Abs(*got-climate.RoundValue(exact)), 1e-9
				if tc.interval == "6h" {
					diff, tol = math.Abs(*got-exact), 0.005+1e-9
				}
				if diff > tol {
					t.Errorf("bucket %d (%s) = %v, want %v (value must describe the span it is labelled with)",
						i, b, *got, climate.RoundValue(exact))
				}
			}
		})
	}
}

// TestSeriesHTTP_RowsShapeSharesTheAlignedAxis covers the other published shape.
// BucketStarts has one caller, so both shapes are fed the same axis — this pins
// that rows does not regress independently.
func TestSeriesHTTP_RowsShapeSharesTheAlignedAxis(t *testing.T) {
	s, q := alignedSetup(t)
	q.QueryFunc = rampingReplay(s, "sensor_a", "24h")

	w := doGET(t, s, "/series?field=temperature_c&fn=mean&group_by=device&window=24h&interval=1h&shape=rows")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Rows []struct {
			Key   string   `json:"key"`
			Time  string   `json:"time"`
			Value *float64 `json:"value"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Rows) == 0 {
		t.Fatal("no rows")
	}
	for i, r := range resp.Rows {
		ts, err := time.Parse(time.RFC3339Nano, r.Time)
		if err != nil {
			t.Fatalf("row %d time %q: %v", i, r.Time, err)
		}
		lt := ts.In(s.Loc)
		if lt.Minute() != 0 || lt.Second() != 0 || lt.Nanosecond() != 0 {
			t.Errorf("row %d time = %q: not on a whole hour", i, r.Time)
		}
	}
}

// TestDeviceSeriesHTTP_RollingWindowIsAligned covers the per-device endpoint,
// which builds its own window and axis through the same helpers.
func TestDeviceSeriesHTTP_RollingWindowIsAligned(t *testing.T) {
	s, q := alignedSetup(t)
	q.QueryFunc = rampingReplay(s, "sensor_a", "48h")

	w := doGET(t, s, "/devices/sensor_a/series?field=temperature_c&fn=mean&window=48h&interval=1h")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp alignedSeriesResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Buckets) == 0 {
		t.Fatal("no buckets")
	}
	if want := "2026-08-16T11:00:00+01:00"; resp.Buckets[0] != want {
		t.Errorf("first bucket = %q, want %q", resp.Buckets[0], want)
	}
	for i, b := range resp.Buckets {
		if len(b) < 19 || b[14:19] != "00:00" {
			t.Errorf("bucket %d = %q: not on a whole hour", i, b)
		}
	}
}

// TestSeriesHTTP_DefaultIntervalRollingIsAligned pins the no-interval request.
// DefaultInterval gives window=24h an interval of 1h, so the bare URL the demo
// pages use was affected too.
func TestSeriesHTTP_DefaultIntervalRollingIsAligned(t *testing.T) {
	s, q := alignedSetup(t)
	q.QueryFunc = rampingReplay(s, "sensor_a", "24h")

	w := doGET(t, s, "/series?field=temperature_c&window=24h")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp alignedSeriesResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Interval != "1h" {
		t.Fatalf("interval = %q, want the 1h default", resp.Interval)
	}
	if want := "2026-08-17T11:00:00+01:00"; resp.Buckets[0] != want {
		t.Errorf("first bucket = %q, want %q", resp.Buckets[0], want)
	}
}

// TestSeriesHTTP_AxisDoesNotMoveWithNow pins the "not stable across refreshes"
// symptom: two requests minutes apart must render the shared history on
// identical labels. Previously the offset was `now mod interval`, so every
// refresh redrew the same data somewhere else.
func TestSeriesHTTP_AxisDoesNotMoveWithNow(t *testing.T) {
	loc := mustLoadLondon(t)
	base := offGridNow(t, loc)

	var reference []string
	for _, drift := range []time.Duration{0, 37 * time.Second, 11 * time.Minute, 41 * time.Minute} {
		s, q := alignedSetup(t)
		s.Clock = testutil.NewFakeClock(base.Add(drift))
		q.QueryFunc = rampingReplay(s, "sensor_a", "24h")

		w := doGET(t, s, "/series?field=temperature_c&fn=mean&window=24h&interval=1h")
		if w.Code != http.StatusOK {
			t.Fatalf("drift %s: want 200, got %d: %s", drift, w.Code, w.Body.String())
		}
		var resp alignedSeriesResp
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if reference == nil {
			reference = resp.Buckets
			continue
		}
		n := len(reference)
		if len(resp.Buckets) < n {
			n = len(resp.Buckets)
		}
		for i := 0; i < n; i++ {
			if resp.Buckets[i] != reference[i] {
				t.Fatalf("drift %s: bucket %d = %q, want %q (the axis must not move with now)",
					drift, i, resp.Buckets[i], reference[i])
			}
		}
	}
}

// TestSeriesHTTP_OnGridWindowsUnchanged is the control from the issue's evidence:
// window=today starts at local midnight, so it was already correct and must stay
// byte-for-byte where it was.
func TestSeriesHTTP_OnGridWindowsUnchanged(t *testing.T) {
	for _, spec := range []string{"today", "7d"} {
		for _, iv := range []string{"1h", "6h"} {
			s, q := alignedSetup(t)
			q.QueryFunc = rampingReplay(s, "sensor_a", spec)

			w := doGET(t, s, fmt.Sprintf("/series?field=temperature_c&fn=mean&window=%s&interval=%s", spec, iv))
			if w.Code != http.StatusOK {
				t.Fatalf("%s/%s: want 200, got %d: %s", spec, iv, w.Code, w.Body.String())
			}
			var resp alignedSeriesResp
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			win, err := climate.ResolveWindow(s.Clock.Now(), s.Loc, spec, time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("ResolveWindow: %v", err)
			}
			want := win.Start.In(s.Loc).Format(time.RFC3339)
			if resp.Buckets[0] != want {
				t.Errorf("%s/%s: first bucket = %q, want the unchanged window start %q", spec, iv, resp.Buckets[0], want)
			}
		}
	}
}

// mustLoadLondon loads the production timezone for tests that need it before a
// Server exists.
func mustLoadLondon(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	return loc
}
