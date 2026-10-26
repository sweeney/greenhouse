package httpapi

import (
	"encoding/json"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/sweeney/greenhouse/internal/config"
	"github.com/sweeney/greenhouse/internal/influx"
	"github.com/sweeney/greenhouse/internal/testutil"
)

// probeReadings is one reading every two minutes over [from, to): a slow wobble
// around base, so every 15-minute bucket has several readings.
func probeReadings(from, to time.Time, base float64) ([]time.Time, []float64) {
	var at []time.Time
	var v []float64
	for t := from; t.Before(to); t = t.Add(2 * time.Minute) {
		at = append(at, t)
		v = append(v, base+0.6*math.Sin(t.Sub(from).Minutes()/41))
	}
	return at, v
}

// TestSeriesHTTP_IssueRequestAcrossTheRepeatedHour replays the exact request
// from issue #36 the day after the autumn change. Prod returned the right
// 193-bucket axis with all three probes null for 01:00Z–01:45Z, the second
// 01:xx. The fake is a GaugeSim: it windows the readings as the flux asks and
// refuses the local sub-day windows Influx stretches across that hour.
func TestSeriesHTTP_IssueRequestAcrossTheRepeatedHour(t *testing.T) {
	s, _ := dataSetup(t)
	now := time.Date(2026, 10, 26, 9, 7, 12, 345678901, time.UTC)
	s.Clock = testutil.NewFakeClock(now)
	probes := map[string]float64{"probe_bigfreezer": -19, "probe_bigfridge": 4, "probe_winefridge": 12}
	devices := map[string]config.DeviceConfig{}
	sim := influx.NewGaugeSim("temperature_c")
	for id, base := range probes {
		devices[id] = config.DeviceConfig{Class: "appliance_probe", DisplayName: id, Room: "kitchen"}
		at, v := probeReadings(now.Add(-50*time.Hour), now, base)
		sim.AddSamples(id, at, v)
	}
	s.Config = fakeConfig{devices: devices}
	s.Influx = sim

	w := doGET(t, s, "/series?field=temperature_c&group_by=device&window=48h&interval=15m"+
		"&devices=probe_bigfreezer,probe_bigfridge,probe_winefridge")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp alignedSeriesResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Buckets) != 193 {
		t.Fatalf("%d buckets, want 193", len(resp.Buckets))
	}
	if len(resp.Series) != 3 {
		t.Fatalf("%d series, want 3", len(resp.Series))
	}
	for _, sr := range resp.Series {
		for i, v := range sr.Values {
			if v == nil {
				t.Errorf("%s: bucket %s is null; the probe reported in it", sr.Key, resp.Buckets[i])
			}
		}
	}
}
