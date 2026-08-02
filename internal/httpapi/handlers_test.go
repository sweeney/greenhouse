package httpapi

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sweeney/greenhouse/internal/climate"
	"github.com/sweeney/greenhouse/internal/config"
	"github.com/sweeney/greenhouse/internal/influx"
	"github.com/sweeney/greenhouse/internal/testutil"
)

// fakeConfig is a static ConfigProvider for handler tests.
type fakeConfig struct {
	devices map[string]config.DeviceConfig
}

func (f fakeConfig) Devices() map[string]config.DeviceConfig { return f.devices }

func testDevices() map[string]config.DeviceConfig {
	return map[string]config.DeviceConfig{
		// No environment_fields → the catalog falls back to the full registry.
		"climate_basement": {
			Class: "environmental_sensor", Location: "basement", DisplayName: "Basement",
		},
		// Explicit hint → the catalog reports exactly these.
		"climate_weatherstation": {
			Class: "environmental_sensor", Location: "garden", DisplayName: "Weather Station",
			EnvironmentFields: []string{"temperature_c", "humidity_pct", "pressure_hpa", "wind_speed_ms"},
		},
		// fire_alarm is charted like any other climate class. It sits in a room
		// holding NO environmental_sensor, mirroring prod (office/utility): the
		// room has no climate coverage at all unless fire alarms are included.
		"firealarm_utility": {
			Class: "fire_alarm", Location: "utility", DisplayName: "Fire Alarm: Utility",
			EnvironmentFields: []string{"temperature_c"},
		},
		// Non-climate: must never appear in the catalog or a series.
		"winefridge": {
			Class: "continuous_power_device", Location: "kitchen", DisplayName: "Wine Fridge",
		},
	}
}

// dataSetup returns a Server wired with a FakeQuerier, a FakeClock fixed to a
// known instant, Loc Europe/London, and a fakeConfig.
func dataSetup(t *testing.T) (*Server, *influx.FakeQuerier) {
	t.Helper()
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	q := &influx.FakeQuerier{PingOK: true}
	s := New(":0", q, nil)
	s.Bucket = "statehouse"
	// Fixed instant: 2026-06-11 14:00:00 BST = 13:00 UTC.
	s.Clock = testutil.NewFakeClock(time.Date(2026, 6, 11, 13, 0, 0, 0, time.UTC))
	s.Loc = loc
	s.Config = fakeConfig{devices: testDevices()}
	return s, q
}

func doGET(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, req)
	return w
}

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode body %q: %v", w.Body.String(), err)
	}
	return m
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// bucketRows builds one row per bucket-start over the window at the given
// interval for a device, all carrying value v.
func bucketRows(t *testing.T, s *Server, deviceID, window, interval string, v float64) []influx.Row {
	t.Helper()
	win, err := climate.ResolveWindow(s.Clock.Now(), s.Loc, window, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("resolve window: %v", err)
	}
	iv, err := climate.ResolveInterval(win, interval, s.Loc)
	if err != nil {
		t.Fatalf("resolve interval: %v", err)
	}
	starts := climate.BucketStarts(win, iv, s.Loc)
	rows := make([]influx.Row, len(starts))
	for i := range starts {
		rows[i] = influx.Row{DeviceID: deviceID, Time: starts[i], Value: v, HasValue: true}
	}
	return rows
}

// --- /devices ---

// catalogResp decodes GET /devices.
type catalogResp struct {
	Devices []struct {
		ID                string   `json:"id"`
		Class             string   `json:"class"`
		Location          string   `json:"location"`
		EnvironmentFields []string `json:"environment_fields"`
	} `json:"devices"`
}

func getCatalog(t *testing.T, s *Server) catalogResp {
	t.Helper()
	w := doGET(t, s, "/devices")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp catalogResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

func TestDevices_OnlyClimate(t *testing.T) {
	s, _ := dataSetup(t)
	resp := getCatalog(t, s)

	// winefridge (continuous_power_device) excluded; the two environmental
	// sensors and the fire alarm remain.
	if len(resp.Devices) != 3 {
		t.Fatalf("want 3 climate devices, got %d: %+v", len(resp.Devices), resp.Devices)
	}
	for _, d := range resp.Devices {
		if d.ID == "winefridge" {
			t.Errorf("non-climate device leaked: %s", d.ID)
		}
		if len(d.EnvironmentFields) == 0 {
			t.Errorf("device %s has no environment_fields hint", d.ID)
		}
	}
}

// A fire_alarm is a first-class climate device: it appears in the catalog and
// carries its own class, so consumers can tell it apart from a purpose-built
// sensor if they want to.
func TestDevices_IncludesFireAlarm(t *testing.T) {
	s, _ := dataSetup(t)
	resp := getCatalog(t, s)

	var found bool
	for _, d := range resp.Devices {
		if d.ID != "firealarm_utility" {
			continue
		}
		found = true
		if d.Class != "fire_alarm" {
			t.Errorf("class = %q, want fire_alarm (class is reported as-is)", d.Class)
		}
		if d.Location != "utility" {
			t.Errorf("location = %q, want utility", d.Location)
		}
		if got := d.EnvironmentFields; len(got) != 1 || got[0] != "temperature_c" {
			t.Errorf("environment_fields = %v, want [temperature_c]", got)
		}
	}
	if !found {
		t.Fatalf("fire alarm missing from catalog: %+v", resp.Devices)
	}
}

// The environment_fields hint is used verbatim when config declares it, and
// falls back to the full registry when it does not. The fallback deliberately
// over-advertises; this test pins both halves so a change to either is visible.
func TestDevices_EnvironmentFieldsHintVsFallback(t *testing.T) {
	s, _ := dataSetup(t)
	resp := getCatalog(t, s)

	byID := map[string][]string{}
	for _, d := range resp.Devices {
		byID[d.ID] = d.EnvironmentFields
	}
	if len(byID["climate_weatherstation"]) != 4 {
		t.Errorf("weatherstation = %v, want the explicit 4 from config", byID["climate_weatherstation"])
	}
	if len(byID["climate_basement"]) != len(climate.FieldNames()) {
		t.Errorf("basement should fall back to the full registry, got %v", byID["climate_basement"])
	}
}

// --- /fields ---

func TestFields(t *testing.T) {
	s, _ := dataSetup(t)
	w := doGET(t, s, "/fields")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var resp struct {
		Fields []struct {
			Name      string `json:"name"`
			Unit      string `json:"unit"`
			DefaultFn string `json:"default_fn"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Fields) != 8 {
		t.Fatalf("want 8 fields, got %d", len(resp.Fields))
	}
	byName := map[string]string{}
	byFn := map[string]string{}
	for _, f := range resp.Fields {
		byName[f.Name] = f.Unit
		byFn[f.Name] = f.DefaultFn
	}
	// Every gauge defaults to mean except the circular bearing, which defaults
	// to last (arithmetic mean of angles is wrong).
	for name, fn := range byFn {
		want := "mean"
		if name == "wind_dir_deg" {
			want = "last"
		}
		if fn != want {
			t.Errorf("%s default_fn = %q, want %q", name, fn, want)
		}
	}
	if byName["temperature_c"] != "°C" {
		t.Errorf("temperature_c unit = %q, want °C", byName["temperature_c"])
	}
}

// --- /devices/{id}/series ---

func TestDeviceSeries_DefaultsTemperatureMean(t *testing.T) {
	s, q := dataSetup(t)
	q.Responses = map[string][]influx.Row{
		`"climate_basement"`: bucketRows(t, s, "climate_basement", "today", "1h", 19.5),
	}
	w := doGET(t, s, "/devices/climate_basement/series?window=today&interval=1h")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	m := decode(t, w)
	if m["field"] != "temperature_c" {
		t.Errorf("field = %v, want temperature_c default", m["field"])
	}
	if m["unit"] != "°C" {
		t.Errorf("unit = %v, want °C", m["unit"])
	}
	if m["fn"] != "mean" {
		t.Errorf("fn = %v, want mean default", m["fn"])
	}
	series := m["series"].([]any)
	if len(series) != 1 {
		t.Fatalf("want 1 series, got %d", len(series))
	}
	// The flux must have requested temperature_c mean.
	if got := q.LastQuery(); got == "" {
		t.Fatal("no query issued")
	}
}

func TestDeviceSeries_UnknownDevice(t *testing.T) {
	s, _ := dataSetup(t)
	w := doGET(t, s, "/devices/nope/series")
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDeviceSeries_NonClimateDevice(t *testing.T) {
	s, _ := dataSetup(t)
	w := doGET(t, s, "/devices/winefridge/series")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	if got := decode(t, w)["error"]; got != "device is not a climate sensor" {
		t.Errorf("error = %v", got)
	}
}

func TestDeviceSeries_UnknownField(t *testing.T) {
	s, _ := dataSetup(t)
	w := doGET(t, s, "/devices/climate_basement/series?field=co2_ppm")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDeviceSeries_InvalidFn(t *testing.T) {
	s, _ := dataSetup(t)
	w := doGET(t, s, "/devices/climate_basement/series?fn=sum")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 (sum is non-additive-forbidden), got %d: %s", w.Code, w.Body.String())
	}
}

func TestDeviceSeries_BadWindow(t *testing.T) {
	s, _ := dataSetup(t)
	w := doGET(t, s, "/devices/climate_basement/series?window=fortnight")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDeviceSeries_Rows(t *testing.T) {
	s, q := dataSetup(t)
	q.Responses = map[string][]influx.Row{
		`"climate_basement"`: bucketRows(t, s, "climate_basement", "today", "1h", 20),
	}
	w := doGET(t, s, "/devices/climate_basement/series?window=today&interval=1h&shape=rows")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	m := decode(t, w)
	if m["shape"] != "rows" {
		t.Errorf("shape = %v, want rows", m["shape"])
	}
	if _, ok := m["rows"]; !ok {
		t.Errorf("rows shape should carry a rows array: %v", m)
	}
}

func TestDeviceSeries_InfluxError(t *testing.T) {
	s, q := dataSetup(t)
	q.Err = errFake
	w := doGET(t, s, "/devices/climate_basement/series")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d: %s", w.Code, w.Body.String())
	}
}

// TestDeviceSeries_WindDirCircular proves the circular-field contract end to
// end: the linear aggregations (mean/min/max) are rejected for wind_dir_deg so
// the API never emits an arithmetic-mean-of-angles bearing, and an unqualified
// request defaults to last (the field's own DefaultFn) rather than the global
// mean, so it resolves instead of 400ing.
func TestDeviceSeries_WindDirCircular(t *testing.T) {
	for _, fn := range []string{"mean", "min", "max"} {
		s, _ := dataSetup(t)
		w := doGET(t, s, "/devices/climate_weatherstation/series?field=wind_dir_deg&fn="+fn)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("fn=%s on circular field: want 400, got %d: %s", fn, w.Code, w.Body.String())
		}
	}

	s, q := dataSetup(t)
	q.QueryFunc = func(string) ([]influx.Row, error) { return nil, nil }
	w := doGET(t, s, "/devices/climate_weatherstation/series?field=wind_dir_deg&window=today&interval=1h")
	if w.Code != http.StatusOK {
		t.Fatalf("default wind_dir_deg request should resolve, got %d: %s", w.Code, w.Body.String())
	}
	if m := decode(t, w); m["fn"] != "last" {
		t.Errorf("wind_dir_deg default fn = %v, want last", m["fn"])
	}
}

// --- /series ---

func TestSeries_GroupByLocationMean(t *testing.T) {
	s, q := dataSetup(t)
	// Two sensors in the same room (office) to prove mean-not-sum end to end.
	devs := testDevices()
	devs["glowsensorth1"] = config.DeviceConfig{Class: "environmental_sensor", Location: "office", DisplayName: "Glow"}
	devs["climate_office"] = config.DeviceConfig{Class: "environmental_sensor", Location: "office", DisplayName: "Office"}
	s.Config = fakeConfig{devices: devs}

	// All devices share one flux (fan-out); return rows for two office members
	// with different values per bucket: 20 and 30 → mean 25.
	rowsA := bucketRows(t, s, "glowsensorth1", "today", "1h", 20)
	rowsB := bucketRows(t, s, "climate_office", "today", "1h", 30)
	q.QueryFunc = func(flux string) ([]influx.Row, error) {
		return append(append([]influx.Row{}, rowsA...), rowsB...), nil
	}

	w := doGET(t, s, "/series?window=today&interval=1h&group_by=location")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		GroupBy string `json:"group_by"`
		Series  []struct {
			Key    string    `json:"key"`
			Values []float64 `json:"values"`
		} `json:"series"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.GroupBy != "location" {
		t.Errorf("group_by = %q", resp.GroupBy)
	}
	var office *struct {
		Key    string    `json:"key"`
		Values []float64 `json:"values"`
	}
	for i := range resp.Series {
		if resp.Series[i].Key == "office" {
			office = &resp.Series[i]
		}
	}
	if office == nil {
		t.Fatalf("office series missing: %+v", resp.Series)
	}
	if !approx(office.Values[0], 25) {
		t.Errorf("office bucket 0 = %v, want 25 (mean of 20 and 30, NOT sum 50)", office.Values[0])
	}
}

// seriesKeys extracts the series keys from a columnar series response.
func seriesKeys(t *testing.T, w *httptest.ResponseRecorder) []string {
	t.Helper()
	var resp struct {
		Series []struct {
			Key string `json:"key"`
		} `json:"series"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	keys := make([]string, len(resp.Series))
	for i, s := range resp.Series {
		keys[i] = s.Key
	}
	return keys
}

func TestSeries_NoFilterAllClimate(t *testing.T) {
	s, q := dataSetup(t)
	q.QueryFunc = func(flux string) ([]influx.Row, error) { return nil, nil }
	w := doGET(t, s, "/series?window=today&interval=1h")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	// Every climate device present — including the fire alarm, which is charted
	// like any other. Non-climate winefridge excluded.
	keys := seriesKeys(t, w)
	want := []string{"climate_basement", "climate_weatherstation", "firealarm_utility"}
	if len(keys) != len(want) {
		t.Fatalf("want %d climate series, got %d: %v", len(want), len(keys), keys)
	}
	for i, k := range want {
		if keys[i] != k {
			t.Errorf("series[%d] = %q, want %q (sorted by id)", i, keys[i], k)
		}
	}
}

// --- fire_alarm as a climate device (option A: class allowlist) ---
//
// These pin the behaviour that makes office/utility visible at all. Before the
// fire_alarm class was charted, every one of these was a 400 or an omission.

// A room whose only environment-reporting device is a fire alarm is reachable
// via locations=. This is the whole point of including the class: without it
// the room has no climate coverage despite live data in Influx.
func TestSeries_LocationsFilter_FireAlarmOnlyRoom(t *testing.T) {
	s, q := dataSetup(t)
	q.QueryFunc = func(flux string) ([]influx.Row, error) {
		return bucketRows(t, s, "firealarm_utility", "today", "1h", 20.21), nil
	}
	w := doGET(t, s, "/series?window=today&interval=1h&locations=utility")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	keys := seriesKeys(t, w)
	if len(keys) != 1 || keys[0] != "firealarm_utility" {
		t.Fatalf("want only firealarm_utility, got %v", keys)
	}
}

// devices= accepts a fire alarm id rather than rejecting it as non-climate.
func TestSeries_DevicesFilter_AcceptsFireAlarm(t *testing.T) {
	s, q := dataSetup(t)
	q.QueryFunc = func(flux string) ([]influx.Row, error) {
		return bucketRows(t, s, "firealarm_utility", "today", "1h", 20.21), nil
	}
	w := doGET(t, s, "/series?window=today&interval=1h&devices=firealarm_utility")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	keys := seriesKeys(t, w)
	if len(keys) != 1 || keys[0] != "firealarm_utility" {
		t.Fatalf("want only firealarm_utility, got %v", keys)
	}
}

// The single-device series endpoint charts a fire alarm.
func TestDeviceSeries_FireAlarm(t *testing.T) {
	s, q := dataSetup(t)
	q.Responses = map[string][]influx.Row{
		`"firealarm_utility"`: bucketRows(t, s, "firealarm_utility", "today", "1h", 20.21),
	}
	w := doGET(t, s, "/devices/firealarm_utility/series?window=today&interval=1h")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	keys := seriesKeys(t, w)
	if len(keys) != 1 || keys[0] != "firealarm_utility" {
		t.Fatalf("want one firealarm_utility series, got %v", keys)
	}
}

// /latest works for a fire alarm too — dashboards treat it as any other sensor.
func TestDeviceLatest_FireAlarm(t *testing.T) {
	s, q := dataSetup(t)
	now := s.Clock.Now()
	q.QueryFunc = func(flux string) ([]influx.Row, error) {
		return []influx.Row{
			{DeviceID: "firealarm_utility", Field: "temperature_c", Value: 20.21, HasValue: true, Time: now},
		}, nil
	}
	w := doGET(t, s, "/devices/firealarm_utility/latest")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Readings []struct {
			Field string  `json:"field"`
			Value float64 `json:"value"`
		} `json:"readings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Readings) != 1 || resp.Readings[0].Field != "temperature_c" {
		t.Fatalf("want one temperature_c reading, got %+v", resp.Readings)
	}
	if !approx(resp.Readings[0].Value, 20.21) {
		t.Errorf("value = %v, want 20.21", resp.Readings[0].Value)
	}
}

// A non-climate device is still rejected — broadening the allowlist must not
// have opened the API to every device in the namespace.
func TestSeries_NonClimateStillRejected(t *testing.T) {
	s, _ := dataSetup(t)
	for _, path := range []string{
		"/series?devices=winefridge",
		"/devices/winefridge/series",
		"/devices/winefridge/latest",
	} {
		w := doGET(t, s, path)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d: %s", path, w.Code, w.Body.String())
		}
	}
}

func TestSeries_DevicesFilter(t *testing.T) {
	s, q := dataSetup(t)
	q.QueryFunc = func(flux string) ([]influx.Row, error) {
		return bucketRows(t, s, "climate_basement", "today", "1h", 19), nil
	}
	w := doGET(t, s, "/series?window=today&interval=1h&devices=climate_basement")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	keys := seriesKeys(t, w)
	if len(keys) != 1 || keys[0] != "climate_basement" {
		t.Fatalf("want only climate_basement, got %v", keys)
	}
}

func TestSeries_LocationsFilter(t *testing.T) {
	s, q := dataSetup(t)
	q.QueryFunc = func(flux string) ([]influx.Row, error) {
		return bucketRows(t, s, "climate_weatherstation", "today", "1h", 15), nil
	}
	w := doGET(t, s, "/series?window=today&interval=1h&locations=garden")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	keys := seriesKeys(t, w)
	if len(keys) != 1 || keys[0] != "climate_weatherstation" {
		t.Fatalf("want only climate_weatherstation (garden), got %v", keys)
	}
}

func TestSeries_FiltersComposeAND(t *testing.T) {
	s, q := dataSetup(t)
	q.QueryFunc = func(flux string) ([]influx.Row, error) {
		return bucketRows(t, s, "climate_weatherstation", "today", "1h", 15), nil
	}
	// Both climate devices requested, but only the garden one survives the
	// location filter — devices= and locations= compose as AND.
	w := doGET(t, s, "/series?window=today&interval=1h&devices=climate_basement,climate_weatherstation&locations=garden")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	keys := seriesKeys(t, w)
	if len(keys) != 1 || keys[0] != "climate_weatherstation" {
		t.Fatalf("want only climate_weatherstation, got %v", keys)
	}
}

func TestSeries_UnknownDeviceFilter(t *testing.T) {
	s, _ := dataSetup(t)
	w := doGET(t, s, "/series?devices=nope")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for unknown device, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSeries_NonClimateDeviceFilter(t *testing.T) {
	s, _ := dataSetup(t)
	// winefridge exists but is a non-climate device — not chartable.
	w := doGET(t, s, "/series?devices=winefridge")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for non-climate device, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSeries_UnknownLocationFilter(t *testing.T) {
	s, _ := dataSetup(t)
	// kitchen holds only winefridge (non-climate), so as far as the climate API
	// is concerned the location does not exist → 400, not a silent empty series.
	w := doGET(t, s, "/series?locations=kitchen")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for climate-free location, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSeries_RollingWindow7d(t *testing.T) {
	s, q := dataSetup(t)
	q.QueryFunc = func(flux string) ([]influx.Row, error) { return nil, nil }
	w := doGET(t, s, "/series?window=7d&devices=climate_basement")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	m := decode(t, w)
	if m["window"] != "7d" {
		t.Errorf("window = %v, want 7d", m["window"])
	}
	// 7d span (~6.6 days at the fixture instant) → 6h smart default.
	if m["interval"] != "6h" {
		t.Errorf("interval = %v, want 6h (span default), proving it is not falling back to today", m["interval"])
	}
	// Fixture now = 2026-06-11 14:00 BST; 7d → local midnight 6 days back.
	if m["from"] != "2026-06-05T00:00:00+01:00" {
		t.Errorf("from = %v, want 2026-06-05T00:00:00+01:00 (day-aligned midnight 6 days back)", m["from"])
	}
	buckets := m["buckets"].([]any)
	if len(buckets) <= 24 {
		t.Errorf("got %d buckets; want > a single day's worth (not silently 'today')", len(buckets))
	}
}

func TestSeries_RollingRejectsFromTo(t *testing.T) {
	s, _ := dataSetup(t)
	// from/to are valid only with window=custom; a rolling window derives its own range.
	w := doGET(t, s, "/series?window=7d&from=2026-01-01T00:00:00Z")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for from with a rolling window, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSeries_BadGroupBy(t *testing.T) {
	s, _ := dataSetup(t)
	w := doGET(t, s, "/series?group_by=class")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 (class not a valid climate group_by), got %d: %s", w.Code, w.Body.String())
	}
}

func TestSeries_HouseGroupRejected(t *testing.T) {
	s, _ := dataSetup(t)
	w := doGET(t, s, "/series?group_by=house")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 (no house group in climate), got %d", w.Code)
	}
}

// TestSeries_FromToOnlyValidWithCustom proves from/to are meaningful ONLY with
// window=custom. Previously a non-custom window parsed from/to and then silently
// discarded them, so a caller who thought they scoped the query got back an
// unrelated range with no error — a quiet correctness/UX trap. The contract is
// now symmetric (from/to <=> custom): the contradictory combination is a 400.
func TestSeries_FromToOnlyValidWithCustom(t *testing.T) {
	from := "2026-06-01T00:00:00Z"
	to := "2026-06-02T00:00:00Z"

	t.Run("week with from/to is rejected", func(t *testing.T) {
		s, _ := dataSetup(t)
		w := doGET(t, s, "/series?window=week&from="+from+"&to="+to)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("from without explicit window (defaults today) is rejected", func(t *testing.T) {
		s, _ := dataSetup(t)
		w := doGET(t, s, "/series?from="+from)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("custom with from/to still resolves to the explicit range", func(t *testing.T) {
		s, q := dataSetup(t)
		q.QueryFunc = func(string) ([]influx.Row, error) { return nil, nil }
		w := doGET(t, s, "/series?window=custom&from="+from+"&to="+to)
		if w.Code != http.StatusOK {
			t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
		}
		if m := decode(t, w); m["window"] != "custom" {
			t.Errorf("window = %v, want custom", m["window"])
		}
	})

	t.Run("today with no from/to still resolves", func(t *testing.T) {
		s, q := dataSetup(t)
		q.QueryFunc = func(string) ([]influx.Row, error) { return nil, nil }
		w := doGET(t, s, "/series?window=today&interval=1h")
		if w.Code != http.StatusOK {
			t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
		}
	})
}

// --- /devices/{id}/latest ---

func TestDeviceLatest(t *testing.T) {
	s, q := dataSetup(t)
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	q.QueryFunc = func(flux string) ([]influx.Row, error) {
		return []influx.Row{
			{DeviceID: "climate_weatherstation", Field: "temperature_c", Value: 21.456, HasValue: true, Time: now},
			{DeviceID: "climate_weatherstation", Field: "humidity_pct", Value: 55.2, HasValue: true, Time: now},
			// Unknown field is skipped.
			{DeviceID: "climate_weatherstation", Field: "co2_ppm", Value: 400, HasValue: true, Time: now},
			// Empty value skipped.
			{DeviceID: "climate_weatherstation", Field: "pressure_hpa", HasValue: false, Time: now},
		}, nil
	}
	w := doGET(t, s, "/devices/climate_weatherstation/latest")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		DeviceID string `json:"device_id"`
		Readings []struct {
			Field string  `json:"field"`
			Unit  string  `json:"unit"`
			Value float64 `json:"value"`
		} `json:"readings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.DeviceID != "climate_weatherstation" {
		t.Errorf("device_id = %q", resp.DeviceID)
	}
	// temperature_c + humidity_pct (co2 unknown, pressure empty both dropped).
	if len(resp.Readings) != 2 {
		t.Fatalf("want 2 readings, got %d: %+v", len(resp.Readings), resp.Readings)
	}
	// Sorted by field: humidity_pct then temperature_c.
	if resp.Readings[0].Field != "humidity_pct" {
		t.Errorf("readings[0].Field = %q, want humidity_pct (sorted)", resp.Readings[0].Field)
	}
	for _, rd := range resp.Readings {
		if rd.Field == "temperature_c" {
			if rd.Unit != "°C" {
				t.Errorf("temp unit = %q", rd.Unit)
			}
			if !approx(rd.Value, 21.46) {
				t.Errorf("temp value = %v, want rounded 21.46", rd.Value)
			}
		}
	}
}

func TestDeviceLatest_NonClimate(t *testing.T) {
	s, _ := dataSetup(t)
	w := doGET(t, s, "/devices/winefridge/latest")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestDeviceLatest_Unknown(t *testing.T) {
	s, _ := dataSetup(t)
	w := doGET(t, s, "/devices/nope/latest")
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

var errFake = &fakeErr{}

type fakeErr struct{}

func (*fakeErr) Error() string { return "boom" }
