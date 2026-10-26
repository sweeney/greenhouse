package influx

import (
	"strings"
	"testing"
	"time"
)

// simFlux renders the aggregateWindow query shape the series builder emits, with
// the location argument spelled out by the test, so the sim is tested against
// the grid a query ASKS for independently of what the builder currently picks.
func simFlux(device, fn, every, location string, start, stop time.Time, createEmpty bool) string {
	return `import "timezone"

from(bucket: "statehouse")
  |> range(start: ` + fluxTime(start) + `, stop: ` + fluxTime(stop) + `)
  |> filter(fn: (r) => r._measurement == "device_environment" and r._field == "temperature_c")
  |> filter(fn: (r) => contains(value: r.device_id, set: ["` + device + `"]))
  |> aggregateWindow(every: ` + every + `, fn: ` + fn + `, timeSrc: "_start", location: ` + location +
		`, createEmpty: ` + map[bool]string{true: "true", false: "false"}[createEmpty] + `)`
}

// minuteRamp returns one reading per minute over [from, to), value = minute index.
func minuteRamp(from, to time.Time) ([]time.Time, []float64) {
	var at []time.Time
	var v []float64
	for i, t := 0, from; t.Before(to); i, t = i+1, t.Add(time.Minute) {
		at = append(at, t)
		v = append(v, float64(i))
	}
	return at, v
}

// TestGaugeSim_RefusesLocalSubDayWindows pins the reason the sim exists (issue
// #36): Influx stretches a sub-day window laid on local time across the autumn
// repeated hour, and a sim that answered one with a fixed step would agree with
// a Go axis that makes the same mistake. It must refuse, naming the issue.
func TestGaugeSim_RefusesLocalSubDayWindows(t *testing.T) {
	start := time.Date(2026, 10, 24, 23, 0, 0, 0, time.UTC)
	stop := start.Add(4 * time.Hour)
	at, v := minuteRamp(start, stop)
	sim := NewGaugeSim("temperature_c").AddSamples("d", at, v)

	for _, every := range []string{"5m", "15m", "30m", "1h", "6h"} {
		_, err := sim.Answer(simFlux("d", "mean", every, `timezone.location(name: "Europe/London")`, start, stop, true))
		if err == nil || !strings.Contains(err.Error(), "issue #36") {
			t.Errorf("every=%s on Europe/London: err = %v, want a refusal naming issue #36", every, err)
		}
	}
	if _, err := sim.Answer(simFlux("d", "mean", "1d", `timezone.location(name: "Europe/London")`, start, stop, true)); err != nil {
		t.Errorf("1d on a named location must be answered: %v", err)
	}
}

// TestGaugeSim_UTCGridAcrossTheRepeatedHour: on timezone.utc every window is
// exactly `every` long, so the repeated hour is four ordinary 15m windows.
func TestGaugeSim_UTCGridAcrossTheRepeatedHour(t *testing.T) {
	start := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC) // 01:00 BST
	stop := time.Date(2026, 10, 25, 2, 0, 0, 0, time.UTC)  // 02:00 GMT
	at, v := minuteRamp(start, stop)
	rows, err := NewGaugeSim("temperature_c").AddSamples("d", at, v).
		Answer(simFlux("d", "mean", "15m", "timezone.utc", start, stop, true))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 8 {
		t.Fatalf("%d rows, want 8 quarter hours over two real hours", len(rows))
	}
	for i, r := range rows {
		if want := start.Add(time.Duration(i) * 15 * time.Minute); !r.Time.Equal(want) {
			t.Errorf("row %d at %s, want %s", i, r.Time, want)
		}
		if want := float64(i*15) + 7; !r.HasValue || r.Value != want {
			t.Errorf("row %d = %v (has=%v), want %v", i, r.Value, r.HasValue, want)
		}
	}
}

// TestGaugeSim_CalendarDayIs25hInAutumn: 1d on a named location is one window
// from local midnight to local midnight, 25 hours on the autumn change.
func TestGaugeSim_CalendarDayIs25hInAutumn(t *testing.T) {
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 25, 0, 0, 0, 0, loc)
	stop := time.Date(2026, 10, 26, 0, 0, 0, 0, loc)
	at, v := minuteRamp(start, stop)
	rows, err := NewGaugeSim("temperature_c").AddSamples("d", at, v).
		Answer(simFlux("d", "count", "1d", `timezone.location(name: "Europe/London")`, start, stop, false))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].Time.Equal(start) || rows[0].Value != 25*60 {
		t.Fatalf("rows = %+v, want one window at %s counting %d minutes", rows, start, 25*60)
	}
}

// TestGaugeSim_TruncatesFirstWindowAndHonoursCreateEmpty covers the two
// aggregateWindow behaviours the demux depends on: an off-grid range start is
// the first row's stamp, and createEmpty decides whether a silent window is a
// value-less row or no row.
func TestGaugeSim_TruncatesFirstWindowAndHonoursCreateEmpty(t *testing.T) {
	start := time.Date(2026, 8, 18, 10, 5, 0, 0, time.UTC)
	stop := time.Date(2026, 8, 18, 13, 0, 0, 0, time.UTC)
	// Readings only in the first and third hours.
	at := []time.Time{start.Add(time.Minute), time.Date(2026, 8, 18, 12, 30, 0, 0, time.UTC)}
	sim := NewGaugeSim("temperature_c").AddSamples("d", at, []float64{1, 3})

	rows, err := sim.Answer(simFlux("d", "last", "1h", "timezone.utc", start, stop, true))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("createEmpty:true: %d rows, want 3", len(rows))
	}
	if !rows[0].Time.Equal(start) {
		t.Errorf("first row stamped %s, want the truncated range start %s", rows[0].Time, start)
	}
	if rows[1].HasValue {
		t.Errorf("silent hour should be an empty row, got %+v", rows[1])
	}

	rows, err = sim.Answer(simFlux("d", "last", "1h", "timezone.utc", start, stop, false))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("createEmpty:false: %d rows, want 2", len(rows))
	}
}

// TestGaugeSim_Aggregates checks every fn over one window, including the two
// the 6h fold reads (sum, count).
func TestGaugeSim_Aggregates(t *testing.T) {
	start := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	stop := start.Add(time.Hour)
	at := []time.Time{start, start.Add(10 * time.Minute), start.Add(50 * time.Minute)}
	sim := NewGaugeSim("temperature_c").AddSamples("d", at, []float64{4, -2, 7})
	for fn, want := range map[string]float64{"mean": 3, "sum": 9, "count": 3, "min": -2, "max": 7, "last": 7} {
		rows, err := sim.Answer(simFlux("d", fn, "1h", "timezone.utc", start, stop, true))
		if err != nil {
			t.Fatalf("%s: %v", fn, err)
		}
		if len(rows) != 1 || rows[0].Value != want {
			t.Errorf("%s: rows = %+v, want one row of %v", fn, rows, want)
		}
	}
	if rows, _ := sim.Answer(strings.Replace(simFlux("d", "mean", "1h", "timezone.utc", start, stop, true),
		`"temperature_c"`, `"humidity_pct"`, 1)); rows != nil {
		t.Errorf("another field must return no rows, got %+v", rows)
	}
}

// TestGaugeSim_CountIsAnIntegerUnlessConverted (PR #37 review): Flux's count is
// an integer, and the client decodes float64 only, so an unconverted count
// arrives with no value. The sim must answer what the flux says, not what the
// caller meant, or dropping the conversion passes every end-to-end test.
func TestGaugeSim_CountIsAnIntegerUnlessConverted(t *testing.T) {
	start := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	stop := start.Add(time.Hour)
	sim := NewGaugeSim("temperature_c").AddSamples("d", []time.Time{start}, []float64{4})

	rows, err := sim.Answer(simFlux("d", "count", "1h", "timezone.utc", start, stop, false))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].HasValue {
		t.Errorf("unconverted count: rows = %+v, want one row with no decodable value", rows)
	}
	rows, err = sim.Answer(simFlux("d", "count", "1h", "timezone.utc", start, stop, false) + "\n  |> toFloat()")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].HasValue || rows[0].Value != 1 {
		t.Errorf("converted count: rows = %+v, want one row of 1", rows)
	}
}
