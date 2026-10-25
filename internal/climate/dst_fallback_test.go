package climate

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sweeney/greenhouse/internal/config"
	"github.com/sweeney/greenhouse/internal/influx"
)

// --- Issue #36: the autumn repeated hour ------------------------------------
//
// On 2026-10-25 (Europe/London, 02:00 BST → 01:00 GMT) prod /series at 15m was
// null for every probe across 01:00Z–01:45Z, the second 01:xx. The fixture
// (testdata/gen_dst_fallback.py) holds one synthetic probe's per-minute readings
// and the rows Influx returns for the location-aware 15m query greenhouse used to
// issue, whose 01:45 BST window runs 75 minutes to 02:00 GMT.

type dstFallbackFixture struct {
	Window struct {
		From time.Time `json:"from"`
		To   time.Time `json:"to"`
	} `json:"window"`
	Interval string `json:"interval"`
	DeviceID string `json:"device_id"`
	Readings []struct {
		Time  time.Time `json:"time"`
		Value float64   `json:"temperature_c"`
	} `json:"readings"`
	FluxRows []struct {
		DeviceID string    `json:"device_id"`
		Field    string    `json:"field"`
		Time     time.Time `json:"time"`
		Value    float64   `json:"value"`
	} `json:"flux_rows"`
}

func loadDSTFallback(t *testing.T) dstFallbackFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/dst_fallback_2026-10-25.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var f dstFallbackFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return f
}

// prodShapedQuerier answers the way prod Influx did: a SUB-DAY query on a named
// location gets the fixture's stretched rows (the shape the sim refuses to
// model), anything else is answered by a GaugeSim over the same readings.
func prodShapedQuerier(f dstFallbackFixture) *influx.FakeQuerier {
	at := make([]time.Time, len(f.Readings))
	v := make([]float64, len(f.Readings))
	for i, r := range f.Readings {
		at[i], v[i] = r.Time, r.Value
	}
	sim := influx.NewGaugeSim("temperature_c").AddSamples(f.DeviceID, at, v)
	return &influx.FakeQuerier{
		PingOK: true,
		QueryFunc: func(flux string) ([]influx.Row, error) {
			if strings.Contains(flux, "timezone.location(") && !strings.Contains(flux, "every: 1d,") {
				rows := make([]influx.Row, len(f.FluxRows))
				for i, r := range f.FluxRows {
					rows[i] = influx.Row{DeviceID: r.DeviceID, Field: r.Field, Time: r.Time, Value: r.Value, HasValue: true}
				}
				return rows, nil
			}
			return sim.Answer(flux)
		},
	}
}

// TestBuildSeries_RepeatedHourIsNotNull is the issue's reproduction: every
// 15-minute bucket across the change, the four inside the repeated hour
// included, carries the mean of its own quarter hour.
func TestBuildSeries_RepeatedHourIsNotNull(t *testing.T) {
	f := loadDSTFallback(t)
	loc := mustLondon(t)
	win := Window{Start: f.Window.From, Stop: f.Window.To, Label: WindowCustom}
	iv := mustInterval(t, f.Interval)
	devices := map[string]config.DeviceConfig{
		f.DeviceID: {Class: "appliance_probe", DisplayName: "Probe A"},
	}

	resp, err := BuildSeries(context.Background(), prodShapedQuerier(f), "statehouse", win, iv,
		"temperature_c", "mean", GroupByDevice, DefaultGroupFn, devices, nil, loc)
	if err != nil {
		t.Fatalf("BuildSeries: %v", err)
	}
	if len(resp.Buckets) != 16 {
		t.Fatalf("%d buckets, want 16 quarter hours over four real hours", len(resp.Buckets))
	}
	got := resp.Series[0].Values
	for i, b := range resp.Buckets {
		want := 0.0
		n := 0
		for _, r := range f.Readings {
			if !r.Time.Before(b) && r.Time.Before(b.Add(iv.Duration)) {
				want += r.Value
				n++
			}
		}
		want /= float64(n)
		repeated := !b.Before(time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC)) && b.Before(time.Date(2026, 10, 25, 2, 0, 0, 0, time.UTC))
		switch {
		case math.IsNaN(got[i]) && repeated:
			t.Errorf("bucket %s (repeated hour) is null; the probe reported %d times in it", b.UTC().Format(time.RFC3339), n)
		case math.IsNaN(got[i]):
			t.Errorf("bucket %s is null; want %.2f", b.UTC().Format(time.RFC3339), want)
		case math.Abs(got[i]-want) > 0.005+1e-9:
			t.Errorf("bucket %s = %v, want %.4f (the mean of its own quarter hour)", b.UTC().Format(time.RFC3339), got[i], want)
		}
	}
}
