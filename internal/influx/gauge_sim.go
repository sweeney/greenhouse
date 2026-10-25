package influx

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

// GaugeSim is a deterministic stand-in for InfluxDB over one device_environment
// field, for use as a Querier (or as a FakeQuerier.QueryFunc via Answer) in tests
// that need the DATABASE's windowing rather than hand-written per-bucket rows.
//
// It holds each device's raw readings and answers the aggregateWindow query
// BuildFieldSeriesFlux emits, honouring what the FLUX asks for, as Influx does:
//
//   - range(start:, stop:) bounds the readings, half-open.
//   - the device set: a device is answered when its id appears quoted in the
//     flux (contains(value: r.device_id, set: [...])).
//   - every: and location: lay the windows (see gaugeSimWindowing).
//   - fn: mean/min/max/last/sum/count, over the readings inside each window.
//   - timeSrc: "_start" stamps each row at its window's LEFT edge, and window
//     bounds are TRUNCATED to the range, so a range that opens mid-window yields
//     a first row stamped at the range start — the off-grid case resolveBucket's
//     snap-back exists for (issue #18).
//   - createEmpty: true emits a value-less row for a window with no reading;
//     false omits it.
//
// Grid (issue #36): a sub-day window on timezone.utc (or with no location) is a
// fixed step on the UTC grid; a 1d window on a named location steps calendar
// days in that zone, so a clock-change day is ONE 23h/25h window. A SUB-DAY
// window on a named location is REFUSED with an error. Influx lays those on
// local wall clock and stretches the window that straddles the autumn repeated
// hour (at 15m the 01:45 BST window runs to 02:00 GMT); the replay harnesses
// this replaces modelled them as a fixed step from local midnight instead —
// exactly the grid the Go axis assumed — so harness and code agreed with each
// other while both disagreed with Influx, and that is how the bug hid. No
// builder emits that shape any more; refusing it makes a regression that
// brings it back fail loudly. Mirrors countinghouse's CounterSim (PR #50).
type GaugeSim struct {
	field  string
	ids    []string // registration order, so rows are returned deterministically
	series map[string]gaugeSeries
}

// gaugeSeries is one device's raw readings, ascending by time.
type gaugeSeries struct {
	at []time.Time
	v  []float64
}

// NewGaugeSim returns an empty sim answering queries for field (e.g.
// "temperature_c"). A query for any other field returns no rows.
func NewGaugeSim(field string) *GaugeSim {
	return &GaugeSim{field: field, series: map[string]gaugeSeries{}}
}

// AddSamples registers deviceID with explicit readings. at must be ascending.
// It returns the sim so registrations can be chained. Mismatched slice lengths
// panic here, naming the call site, rather than as an index panic inside a
// query later.
func (g *GaugeSim) AddSamples(deviceID string, at []time.Time, v []float64) *GaugeSim {
	if len(at) != len(v) {
		panic(fmt.Sprintf("influx: GaugeSim.AddSamples(%q): %d times but %d readings", deviceID, len(at), len(v)))
	}
	for i := 1; i < len(at); i++ {
		if !at[i].After(at[i-1]) {
			panic(fmt.Sprintf("influx: GaugeSim.AddSamples(%q): reading %d (%s) is not after %s", deviceID, i, at[i], at[i-1]))
		}
	}
	if _, seen := g.series[deviceID]; !seen {
		g.ids = append(g.ids, deviceID)
	}
	g.series[deviceID] = gaugeSeries{at: at, v: v}
	return g
}

// Query implements Querier.
func (g *GaugeSim) Query(_ context.Context, flux string) ([]Row, error) { return g.Answer(flux) }

// Ping implements Querier; the sim is always reachable.
func (g *GaugeSim) Ping(context.Context) bool { return true }

var (
	simRangeRe       = regexp.MustCompile(`range\(start: (\S+), stop: (\S+)\)`)
	simEveryRe       = regexp.MustCompile(`every: (\w+),`)
	simFnRe          = regexp.MustCompile(`aggregateWindow\(every: \w+, fn: (\w+),`)
	simLocationRe    = regexp.MustCompile(`location: (timezone\.utc|timezone\.location\(name: "([^"]+)"\))`)
	simCreateEmptyRe = regexp.MustCompile(`createEmpty: (true|false)`)
)

// gaugeSimWindowing reads how an aggregateWindow call asks for its windows: the
// width (every:) and the zone whose grid they are laid on (location:).
//
//   - location: timezone.utc, or none (Flux's default) → the UTC grid.
//   - location: timezone.location(name: "X") with every: 1d → calendar days in X.
//   - a named location with a SUB-DAY every: → an error (see GaugeSim).
func gaugeSimWindowing(flux string) (time.Duration, *time.Location, error) {
	m := simEveryRe.FindStringSubmatch(flux)
	if m == nil {
		return 0, nil, fmt.Errorf("influx: GaugeSim: aggregateWindow with no every: in flux:\n%s", flux)
	}
	every, err := simDuration(m[1])
	if err != nil {
		return 0, nil, fmt.Errorf("influx: GaugeSim: unsupported interval %q: %w", m[1], err)
	}
	lm := simLocationRe.FindStringSubmatch(flux)
	if lm == nil || lm[1] == "timezone.utc" {
		return every, time.UTC, nil
	}
	if every < 24*time.Hour {
		return 0, nil, fmt.Errorf("influx: GaugeSim: refusing a sub-day aggregateWindow (every: %s) on local time %q: "+
			"Influx stretches those across a clock change and the sim does not model it; "+
			"sub-day series must window on timezone.utc (issue #36)", m[1], lm[2])
	}
	loc, err := time.LoadLocation(lm[2])
	if err != nil {
		return 0, nil, fmt.Errorf("influx: GaugeSim: %w", err)
	}
	return every, loc, nil
}

// Answer resolves one Flux script against the registered readings. Its
// signature matches FakeQuerier.QueryFunc, so a test wanting FakeQuerier's
// query recording can wire it in as `&FakeQuerier{QueryFunc: sim.Answer}`.
func (g *GaugeSim) Answer(flux string) ([]Row, error) {
	if !strings.Contains(flux, fmt.Sprintf("r._field == %q", g.field)) {
		return nil, nil // not this sim's field
	}
	if !strings.Contains(flux, "aggregateWindow") {
		return nil, fmt.Errorf("influx: GaugeSim: only aggregateWindow queries are modelled:\n%s", flux)
	}
	rm := simRangeRe.FindStringSubmatch(flux)
	if rm == nil {
		return nil, fmt.Errorf("influx: GaugeSim: no range(start:, stop:) in flux:\n%s", flux)
	}
	start, err := time.Parse(time.RFC3339, rm[1])
	if err != nil {
		return nil, fmt.Errorf("influx: GaugeSim: range start: %w", err)
	}
	stop, err := time.Parse(time.RFC3339, rm[2])
	if err != nil {
		return nil, fmt.Errorf("influx: GaugeSim: range stop: %w", err)
	}
	every, loc, err := gaugeSimWindowing(flux)
	if err != nil {
		return nil, err
	}
	fm := simFnRe.FindStringSubmatch(flux)
	if fm == nil {
		return nil, fmt.Errorf("influx: GaugeSim: aggregateWindow with no fn: in flux:\n%s", flux)
	}
	agg, ok := gaugeSimFns[fm[1]]
	if !ok {
		return nil, fmt.Errorf("influx: GaugeSim: unsupported fn %q", fm[1])
	}
	createEmpty := false
	if cm := simCreateEmptyRe.FindStringSubmatch(flux); cm != nil {
		createEmpty = cm[1] == "true"
	}

	var rows []Row
	for _, id := range g.ids {
		if !strings.Contains(flux, `"`+id+`"`) {
			continue
		}
		s := g.series[id]
		if !s.anyIn(start, stop) {
			continue // no table for this device at all, as in Influx
		}
		for lo := simGridStart(start, every, loc); lo.Before(stop); {
			hi := simGridNext(lo, every, loc)
			wlo, whi := lo, hi
			if wlo.Before(start) {
				wlo = start
			}
			if whi.After(stop) {
				whi = stop
			}
			if whi.After(wlo) {
				vals := s.in(wlo, whi)
				row := Row{DeviceID: id, Field: g.field, Time: wlo}
				if v, ok := agg(vals); ok {
					row.Value, row.HasValue = v, true
					rows = append(rows, row)
				} else if createEmpty {
					rows = append(rows, row)
				}
			}
			lo = hi
		}
	}
	return rows, nil
}

// simDuration parses a Flux duration literal of the shapes the builders emit
// (5m, 1h, 6h, 1d). It is deliberately the sim's own parser rather than the
// builders' table, so the sim does not share the code it is testing.
func simDuration(tok string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(tok, "d"); ok {
		d, err := time.ParseDuration(n + "h")
		return 24 * d, err
	}
	return time.ParseDuration(tok)
}

// simGridStart is the grid boundary at or before start: UTC-truncated for a
// sub-day grid, local midnight of start's date for a calendar day.
func simGridStart(start time.Time, every time.Duration, loc *time.Location) time.Time {
	if every >= 24*time.Hour {
		ls := start.In(loc)
		return time.Date(ls.Year(), ls.Month(), ls.Day(), 0, 0, 0, 0, loc)
	}
	return start.Truncate(every)
}

// simGridNext is the boundary after lo: a fixed step on the UTC grid, or the
// next local midnight for a calendar day.
func simGridNext(lo time.Time, every time.Duration, loc *time.Location) time.Time {
	if every >= 24*time.Hour {
		l := lo.In(loc)
		return time.Date(l.Year(), l.Month(), l.Day()+1, 0, 0, 0, 0, loc)
	}
	return lo.Add(every)
}

func (s gaugeSeries) anyIn(lo, hi time.Time) bool { return len(s.in(lo, hi)) > 0 }

// in returns the readings in [lo, hi).
func (s gaugeSeries) in(lo, hi time.Time) []float64 {
	var out []float64
	for i, at := range s.at {
		if !at.Before(lo) && at.Before(hi) {
			out = append(out, s.v[i])
		}
	}
	return out
}

// gaugeSimFns are Flux's aggregates over one window's readings. The bool is
// false where Flux yields a null (an empty window); count of nothing is 0, as
// in Flux, which is why it reports true.
var gaugeSimFns = map[string]func([]float64) (float64, bool){
	"mean": func(v []float64) (float64, bool) {
		if len(v) == 0 {
			return 0, false
		}
		s := 0.0
		for _, x := range v {
			s += x
		}
		return s / float64(len(v)), true
	},
	"sum": func(v []float64) (float64, bool) {
		if len(v) == 0 {
			return 0, false
		}
		s := 0.0
		for _, x := range v {
			s += x
		}
		return s, true
	},
	"count": func(v []float64) (float64, bool) { return float64(len(v)), true },
	"min": func(v []float64) (float64, bool) {
		if len(v) == 0 {
			return 0, false
		}
		m := math.Inf(1)
		for _, x := range v {
			m = math.Min(m, x)
		}
		return m, true
	},
	"max": func(v []float64) (float64, bool) {
		if len(v) == 0 {
			return 0, false
		}
		m := math.Inf(-1)
		for _, x := range v {
			m = math.Max(m, x)
		}
		return m, true
	},
	"last": func(v []float64) (float64, bool) {
		if len(v) == 0 {
			return 0, false
		}
		return v[len(v)-1], true
	},
}
