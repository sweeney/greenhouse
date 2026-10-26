package climate

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/sweeney/greenhouse/internal/config"
	"github.com/sweeney/greenhouse/internal/influx"
)

// TestBuildSeries_6hMeanSumAndCountDescribeTheSameReadings (PR #37 review): a
// 6h mean is Σsum/Σcount over its hours, and both numbers must come from ONE
// read of the data. statehouse keeps writing readings stamped just before
// "now", so a reading that lands between two separate round trips is counted
// in one and missing from the other. Early in the open bucket the count is
// small, so the error is large: with every reading 20.0 °C the open bucket read
// 10. The double here adds a reading after the first query it answers, which
// is what a live Influx does to any second query.
func TestBuildSeries_6hMeanSumAndCountDescribeTheSameReadings(t *testing.T) {
	loc := mustLondon(t)
	now := time.Date(2026, 7, 1, 6, 20, 0, 0, loc)
	win, err := ResolveWindow(now, loc, "24h", time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	iv := mustInterval(t, "6h")
	const dev = "sensor_a"
	first := time.Date(2026, 7, 1, 6, 10, 0, 0, loc)
	late := time.Date(2026, 7, 1, 6, 19, 0, 0, loc)

	sim := influx.NewGaugeSim("temperature_c").AddSamples(dev, []time.Time{first}, []float64{20})
	var mu sync.Mutex
	calls := 0
	q := &influx.FakeQuerier{PingOK: true, QueryFunc: func(flux string) ([]influx.Row, error) {
		mu.Lock()
		defer mu.Unlock()
		rows, err := sim.Answer(flux)
		if calls++; calls == 1 {
			// The 06:19 reading becomes visible only after the first read.
			sim.AddSamples(dev, []time.Time{first, late}, []float64{20, 20})
		}
		return rows, err
	}}
	devices := map[string]config.DeviceConfig{dev: {Class: "environmental_sensor", DisplayName: "Sensor A"}}

	resp, err := BuildSeries(context.Background(), q, "statehouse", win, iv, "temperature_c", "mean",
		GroupByDevice, DefaultGroupFn, devices, nil, loc)
	if err != nil {
		t.Fatalf("BuildSeries: %v", err)
	}
	vals := resp.Series[0].Values
	last := len(vals) - 1
	if got := vals[last]; math.IsNaN(got) || math.Abs(got-20) > 1e-9 {
		t.Errorf("open bucket %s = %v after %d queries; every reading was 20.0 °C",
			resp.Buckets[last], got, calls)
	}
}
