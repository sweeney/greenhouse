package influx

import (
	"strings"
	"testing"
	"time"
)

// sumCountFlux is the single-pass sum+count shape: window() then reduce(), with
// the identity and increment literals supplied by the test.
func sumCountFlux(device, location, identity, increment string, start, stop time.Time) string {
	return `import "timezone"

from(bucket: "statehouse")
  |> range(start: ` + fluxTime(start) + `, stop: ` + fluxTime(stop) + `)
  |> filter(fn: (r) => r._measurement == "device_environment" and r._field == "temperature_c")
  |> filter(fn: (r) => contains(value: r.device_id, set: ["` + device + `"]))
  |> window(every: 1h, location: ` + location + `)
  |> reduce(identity: ` + identity + `, fn: (r, accumulator) => ({sum: accumulator.sum + float(v: r._value), count: accumulator.count + ` + increment + `}))
  |> duplicate(column: "_start", as: "_time")
  |> window(every: inf)`
}

// TestGaugeSim_SumCountInOnePass models the reduce() shape the 6h mean fold
// reads: one row per hour carrying BOTH sum and count, from the same readings.
func TestGaugeSim_SumCountInOnePass(t *testing.T) {
	start := time.Date(2026, 8, 18, 10, 5, 0, 0, time.UTC)
	stop := time.Date(2026, 8, 18, 13, 0, 0, 0, time.UTC)
	at := []time.Time{start.Add(time.Minute), start.Add(2 * time.Minute), time.Date(2026, 8, 18, 12, 30, 0, 0, time.UTC)}
	sim := NewGaugeSim("temperature_c").AddSamples("d", at, []float64{1, 2, 7})

	rows, err := sim.Answer(sumCountFlux("d", "timezone.utc", "{sum: 0.0, count: 0.0}", "1.0", start, stop))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("%d rows, want 2 (a window with no reading is omitted): %+v", len(rows), rows)
	}
	if r := rows[0]; !r.Time.Equal(start) || !r.HasSumCount || r.Sum != 3 || r.Count != 2 {
		t.Errorf("first row = %+v, want sum 3 count 2 stamped at the truncated range start", r)
	}
	if r := rows[1]; !r.Time.Equal(time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)) || r.Sum != 7 || r.Count != 1 {
		t.Errorf("second row = %+v, want sum 7 count 1 at 12:00Z", r)
	}

	// An integer count is not decodable, exactly as for aggregateWindow.
	rows, err = sim.Answer(sumCountFlux("d", "timezone.utc", "{sum: 0.0, count: 0}", "1", start, stop))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.HasSumCount {
			t.Errorf("integer count decoded as a value: %+v", r)
		}
	}

	// Local sub-day windows are refused in this shape too.
	if _, err := sim.Answer(sumCountFlux("d", `timezone.location(name: "Europe/London")`, "{sum: 0.0, count: 0.0}", "1.0", start, stop)); err == nil || !strings.Contains(err.Error(), "issue #36") {
		t.Errorf("local sub-day window: err = %v, want a refusal naming issue #36", err)
	}
}
