package climate

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/sweeney/greenhouse/internal/config"
	"github.com/sweeney/greenhouse/internal/influx"
)

func envDevices() map[string]config.DeviceConfig {
	return map[string]config.DeviceConfig{
		"climate_basement":    {Class: "environmental_sensor", Location: "basement", DisplayName: "Basement"},
		"climate_groundfloor": {Class: "environmental_sensor", Location: "groundfloor", DisplayName: "Ground Floor"},
		// Two sensors share the "office" location so group_by=location must mean them.
		"glowsensorth1":  {Class: "environmental_sensor", Location: "office", DisplayName: "Glow Sensor"},
		"climate_office": {Class: "environmental_sensor", Location: "office", DisplayName: "Office"},
		// A non-environmental device must be ignored entirely.
		"winefridge": {Class: "continuous_power_device", Location: "kitchen", DisplayName: "Wine Fridge"},
	}
}

// vals builds a per-device value map for AssembleSeries from explicit slices.
func vals(m map[string][]float64) map[string][]float64 { return m }

func TestAssembleSeries_ByDevice(t *testing.T) {
	buckets := make([]time.Time, 3)
	base := time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC)
	for i := range buckets {
		buckets[i] = base.Add(time.Duration(i) * time.Hour)
	}
	devices := envDevices()
	v := vals(map[string][]float64{
		"climate_basement":    {18, 19, 20},
		"climate_groundfloor": {21, 22, 23},
		"glowsensorth1":       {25, 26, 27},
		"climate_office":      {30, 31, 32},
	})

	got := AssembleSeries(buckets, devices, v, GroupByDevice)
	// 4 environmental devices, winefridge excluded; sorted by id.
	if len(got) != 4 {
		t.Fatalf("want 4 device series, got %d", len(got))
	}
	wantKeys := []string{"climate_basement", "climate_groundfloor", "climate_office", "glowsensorth1"}
	for i, k := range wantKeys {
		if got[i].Key != k {
			t.Errorf("series[%d].Key = %q, want %q", i, got[i].Key, k)
		}
	}
	if got[0].Values[0] != 18 || got[0].Values[2] != 20 {
		t.Errorf("basement values = %v", got[0].Values)
	}
	if got[0].Room != "basement" {
		t.Errorf("basement room = %q", got[0].Room)
	}
	// Summary stats.
	if got[0].Min != 18 || got[0].Max != 20 || got[0].Mean != 19 {
		t.Errorf("basement stats min/max/mean = %v/%v/%v", got[0].Min, got[0].Max, got[0].Mean)
	}
}

// TestAssembleSeries_ByRoomMeansNotSums is the defining greenhouse test:
// two sensors in the same room combine as the MEAN of their readings, never the
// sum. (Summing temperatures would be physically meaningless.)
func TestAssembleSeries_ByRoomMeansNotSums(t *testing.T) {
	buckets := make([]time.Time, 2)
	base := time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC)
	for i := range buckets {
		buckets[i] = base.Add(time.Duration(i) * time.Hour)
	}
	devices := envDevices()
	v := vals(map[string][]float64{
		"climate_basement":    {18, 19},
		"climate_groundfloor": {21, 22},
		"glowsensorth1":       {20, 30}, // office member A
		"climate_office":      {30, 10}, // office member B
	})

	got := AssembleSeries(buckets, devices, v, GroupByRoom)
	// Locations: basement, groundfloor, office (sorted).
	byKey := map[string]Series{}
	for _, s := range got {
		byKey[s.Key] = s
	}
	office, ok := byKey["office"]
	if !ok {
		t.Fatalf("office location series missing: %+v", got)
	}
	// MEAN: (20+30)/2 = 25 ; (30+10)/2 = 20. A SUM would give 50 and 40.
	if office.Values[0] != 25 || office.Values[1] != 20 {
		t.Errorf("office mean values = %v, want [25 20] (mean, NOT sum)", office.Values)
	}
	if office.Values[0] == 50 || office.Values[1] == 40 {
		t.Fatal("office is summing readings — must be mean")
	}
	// Single-member rooms pass through unchanged.
	if byKey["basement"].Values[0] != 18 {
		t.Errorf("basement value = %v, want 18", byKey["basement"].Values[0])
	}
}

func TestAssembleSeries_GapsArePreserved(t *testing.T) {
	buckets := make([]time.Time, 3)
	base := time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC)
	for i := range buckets {
		buckets[i] = base.Add(time.Duration(i) * time.Hour)
	}
	devices := map[string]config.DeviceConfig{
		"climate_basement": {Class: "environmental_sensor", Location: "basement"},
	}
	v := vals(map[string][]float64{
		"climate_basement": {18, math.NaN(), 20},
	})
	got := AssembleSeries(buckets, devices, v, GroupByDevice)
	if len(got) != 1 {
		t.Fatalf("want 1 series, got %d", len(got))
	}
	if !math.IsNaN(got[0].Values[1]) {
		t.Errorf("gap bucket should be NaN, got %v", got[0].Values[1])
	}
	// Stats ignore the gap: min 18, max 20, mean 19.
	if got[0].Min != 18 || got[0].Max != 20 || got[0].Mean != 19 {
		t.Errorf("stats over gap = %v/%v/%v", got[0].Min, got[0].Max, got[0].Mean)
	}
}

func TestSeries_MarshalNaNAsNull(t *testing.T) {
	s := Series{Key: "k", Label: "l", Values: []float64{1.5, math.NaN(), 2.5}, Min: 1.5, Max: 2.5, Mean: 2.0}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := string(b)
	if !strings.Contains(out, `"values":[1.5,null,2.5]`) {
		t.Errorf("NaN not rendered as null: %s", out)
	}
}

func TestSeries_MarshalEmptyStatsAsNull(t *testing.T) {
	s := Series{Key: "k", Label: "l", Values: []float64{math.NaN()}, Min: math.NaN(), Max: math.NaN(), Mean: math.NaN()}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal (NaN would be invalid JSON): %v", err)
	}
	if decoded["mean"] != nil {
		t.Errorf("empty-series mean should be null, got %v", decoded["mean"])
	}
}

// bucketedRows builds influx.Rows for a device with one row per bucket start.
// Values with NaN are emitted as empty (HasValue=false) buckets.
func bucketedRows(deviceID string, starts []time.Time, values []float64) []influx.Row {
	rows := make([]influx.Row, len(starts))
	for i := range starts {
		r := influx.Row{DeviceID: deviceID, Time: starts[i]}
		if !math.IsNaN(values[i]) {
			r.Value = values[i]
			r.HasValue = true
		}
		rows[i] = r
	}
	return rows
}

func TestBuildSeries_SingleDeviceMeanField(t *testing.T) {
	loc := mustLondon(t)
	now := time.Date(2026, 6, 11, 3, 0, 0, 0, time.UTC) // 04:00 BST
	win, _ := ResolveWindow(now, loc, WindowToday, time.Time{}, time.Time{})
	iv, _ := ResolveInterval(win, "1h", loc)
	buckets := BucketStarts(win, iv, loc)

	devices := map[string]config.DeviceConfig{
		"climate_basement": {Class: "environmental_sensor", Location: "basement", DisplayName: "Basement"},
	}
	temps := make([]float64, len(buckets))
	for i := range temps {
		temps[i] = 18 + float64(i)
	}
	q := &influx.FakeQuerier{
		PingOK: true,
		Responses: map[string][]influx.Row{
			`"climate_basement"`: bucketedRows("climate_basement", buckets, temps),
		},
	}

	resp, err := BuildSeries(context.Background(), q, "statehouse", win, iv, "temperature_c", "mean", GroupByDevice, devices, loc)
	if err != nil {
		t.Fatalf("BuildSeries: %v", err)
	}
	if resp.Field != "temperature_c" {
		t.Errorf("field = %q", resp.Field)
	}
	if resp.Unit != "°C" {
		t.Errorf("unit = %q, want °C", resp.Unit)
	}
	if resp.Fn != "mean" {
		t.Errorf("fn = %q", resp.Fn)
	}
	if len(resp.Series) != 1 {
		t.Fatalf("want 1 series, got %d", len(resp.Series))
	}
	if len(resp.Series[0].Values) != len(buckets) {
		t.Errorf("values len %d, buckets %d", len(resp.Series[0].Values), len(buckets))
	}
	if resp.Series[0].Values[0] != 18 {
		t.Errorf("first bucket = %v, want 18", resp.Series[0].Values[0])
	}
	// The flux must request the chosen field and fn.
	flux := q.LastQuery()
	if !strings.Contains(flux, `r._field == "temperature_c"`) || !strings.Contains(flux, "fn: mean,") {
		t.Errorf("flux did not parameterise field/fn:\n%s", flux)
	}
}

func TestBuildSeries_FnPassThrough(t *testing.T) {
	loc := mustLondon(t)
	now := time.Date(2026, 6, 11, 3, 0, 0, 0, time.UTC)
	win, _ := ResolveWindow(now, loc, WindowToday, time.Time{}, time.Time{})
	iv, _ := ResolveInterval(win, "1h", loc)
	devices := map[string]config.DeviceConfig{
		"climate_basement": {Class: "environmental_sensor"},
	}
	for _, fn := range []string{"min", "max", "last"} {
		q := &influx.FakeQuerier{PingOK: true}
		_, err := BuildSeries(context.Background(), q, "statehouse", win, iv, "humidity_pct", fn, GroupByDevice, devices, loc)
		if err != nil {
			t.Fatalf("BuildSeries fn=%s: %v", fn, err)
		}
		if !strings.Contains(q.LastQuery(), "fn: "+fn+",") {
			t.Errorf("fn %s not in flux:\n%s", fn, q.LastQuery())
		}
	}
}

// TestBuildSeries_DemuxLeftEdgeAndInterior guards bucket alignment. With the
// mandated timeSrc:"_start", real rows land exactly on bucket starts (left
// edge) and must map to that bucket. A non-exact interior stamp (a reading a
// few minutes into a bucket) snaps back to its containing bucket; a stamp before
// the first bucket is dropped.
func TestBuildSeries_DemuxLeftEdgeAndInterior(t *testing.T) {
	loc := time.UTC
	win := Window{
		Start: time.Date(2026, 6, 11, 0, 0, 0, 0, loc),
		Stop:  time.Date(2026, 6, 11, 3, 0, 0, 0, loc),
		Label: WindowCustom,
	}
	iv, _ := ResolveInterval(win, "1h", loc)
	buckets := BucketStarts(win, iv, loc)
	if len(buckets) != 3 {
		t.Fatalf("want 3 buckets, got %d", len(buckets))
	}

	rows := []influx.Row{
		// Left-edge (exact) stamps for buckets 0 and 2.
		{DeviceID: "d", Time: buckets[0], Value: 10, HasValue: true},
		{DeviceID: "d", Time: buckets[2], Value: 12, HasValue: true},
		// Interior stamp inside bucket 1 (30 min in) snaps to bucket 1.
		{DeviceID: "d", Time: buckets[1].Add(30 * time.Minute), Value: 11, HasValue: true},
		// Before the window: dropped.
		{DeviceID: "d", Time: buckets[0].Add(-time.Hour), Value: 99, HasValue: true},
	}
	idx := bucketIndex(buckets)
	dst := map[string][]float64{}
	demux(rows, idx, dst, len(buckets))

	got := dst["d"]
	want := []float64{10, 11, 12}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("bucket %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestBuildSeries_QueryError(t *testing.T) {
	loc := mustLondon(t)
	now := time.Date(2026, 6, 11, 3, 0, 0, 0, time.UTC)
	win, _ := ResolveWindow(now, loc, WindowToday, time.Time{}, time.Time{})
	iv, _ := ResolveInterval(win, "1h", loc)
	devices := map[string]config.DeviceConfig{"climate_basement": {Class: "environmental_sensor"}}
	q := &influx.FakeQuerier{Err: context.DeadlineExceeded}
	if _, err := BuildSeries(context.Background(), q, "statehouse", win, iv, "temperature_c", "mean", GroupByDevice, devices, loc); err == nil {
		t.Fatal("expected error from query failure")
	}
}

func TestBuildSeries_NoEnvironmentalDevicesNoQuery(t *testing.T) {
	loc := mustLondon(t)
	now := time.Date(2026, 6, 11, 3, 0, 0, 0, time.UTC)
	win, _ := ResolveWindow(now, loc, WindowToday, time.Time{}, time.Time{})
	iv, _ := ResolveInterval(win, "1h", loc)
	devices := map[string]config.DeviceConfig{"winefridge": {Class: "continuous_power_device"}}
	q := &influx.FakeQuerier{PingOK: true}
	resp, err := BuildSeries(context.Background(), q, "statehouse", win, iv, "temperature_c", "mean", GroupByDevice, devices, loc)
	if err != nil {
		t.Fatalf("BuildSeries: %v", err)
	}
	if len(q.Queries) != 0 {
		t.Errorf("no environmental devices should issue no query, got %d", len(q.Queries))
	}
	if len(resp.Series) != 0 {
		t.Errorf("want 0 series, got %d", len(resp.Series))
	}
}

// --- fire_alarm participation (option A: class allowlist) ---

// mixedClassDevices mirrors the real network_cabinet topology: a purpose-built
// sensor and a fire alarm in the SAME room, plus a fire alarm alone in a room
// with no environmental_sensor at all (office/utility in prod).
func mixedClassDevices() map[string]config.DeviceConfig {
	return map[string]config.DeviceConfig{
		"glowsensorth1":     {Class: "environmental_sensor", Location: "network_cabinet", DisplayName: "Glow Sensor"},
		"firealarm_network": {Class: "fire_alarm", Location: "network_cabinet", DisplayName: "Fire Alarm: Network"},
		"firealarm_utility": {Class: "fire_alarm", Location: "utility", DisplayName: "Fire Alarm: Utility"},
		"winefridge":        {Class: "continuous_power_device", Location: "kitchen", DisplayName: "Wine Fridge"},
	}
}

// group_by=device keeps every climate device on its own line regardless of
// class, and still excludes non-climate devices.
func TestAssembleSeries_ByDevice_IncludesFireAlarms(t *testing.T) {
	buckets := []time.Time{time.Unix(0, 0).UTC(), time.Unix(3600, 0).UTC()}
	got := AssembleSeries(buckets, mixedClassDevices(), vals(map[string][]float64{
		"glowsensorth1":     {22.7, 22.8},
		"firealarm_network": {23.6, 23.5},
		"firealarm_utility": {20.2, 20.3},
		"winefridge":        {99, 99},
	}), GroupByDevice)

	var keys []string
	for _, s := range got {
		keys = append(keys, s.Key)
	}
	want := []string{"firealarm_network", "firealarm_utility", "glowsensorth1"}
	if len(keys) != len(want) {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Errorf("keys[%d] = %q, want %q", i, keys[i], want[i])
		}
	}
}

// group_by=location means a fire alarm together with a co-located sensor —
// the first time any real room has had two members. Mean, never sum: this is
// the non-additive invariant applied across mixed classes.
func TestAssembleSeries_ByLocation_MeansAcrossMixedClasses(t *testing.T) {
	buckets := []time.Time{time.Unix(0, 0).UTC()}
	got := AssembleSeries(buckets, mixedClassDevices(), vals(map[string][]float64{
		"glowsensorth1":     {22.0},
		"firealarm_network": {24.0},
		"firealarm_utility": {20.0},
	}), GroupByRoom)

	byKey := map[string]Series{}
	for _, s := range got {
		byKey[s.Key] = s
	}
	cab, ok := byKey["network_cabinet"]
	if !ok {
		t.Fatalf("network_cabinet missing: %+v", got)
	}
	if math.Abs(cab.Values[0]-23.0) > 1e-9 {
		t.Errorf("network_cabinet = %v, want 23 (mean of 22 and 24, NOT sum 46)", cab.Values[0])
	}
	// A fire-alarm-only room still yields its own location series.
	util, ok := byKey["utility"]
	if !ok {
		t.Fatalf("utility missing — a room whose only sensor is a fire alarm must still appear: %+v", got)
	}
	if math.Abs(util.Values[0]-20.0) > 1e-9 {
		t.Errorf("utility = %v, want 20", util.Values[0])
	}
	if _, leaked := byKey["kitchen"]; leaked {
		t.Error("kitchen leaked: winefridge is not a climate device")
	}
}
