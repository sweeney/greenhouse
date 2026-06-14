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
		"climate_basement": {
			Class: "environmental_sensor", Location: "basement", DisplayName: "Basement",
		},
		"climate_weatherstation": {
			Class: "environmental_sensor", Location: "garden", DisplayName: "Weather Station",
			Fields: []string{"temperature_c", "humidity_pct", "pressure_hpa", "wind_speed_ms"},
		},
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

func TestDevices_OnlyClimate(t *testing.T) {
	s, _ := dataSetup(t)
	w := doGET(t, s, "/devices")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Devices []struct {
			ID     string   `json:"id"`
			Class  string   `json:"class"`
			Fields []string `json:"fields"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// winefridge (continuous_power_device) excluded; 2 climate devices remain.
	if len(resp.Devices) != 2 {
		t.Fatalf("want 2 climate devices, got %d: %+v", len(resp.Devices), resp.Devices)
	}
	for _, d := range resp.Devices {
		if d.Class != "environmental_sensor" {
			t.Errorf("non-climate device leaked: %s", d.ID)
		}
		if len(d.Fields) == 0 {
			t.Errorf("device %s has no fields hint", d.ID)
		}
	}
	// Weather station's explicit fields hint is used; basement falls back to registry.
	byID := map[string][]string{}
	for _, d := range resp.Devices {
		byID[d.ID] = d.Fields
	}
	if len(byID["climate_weatherstation"]) != 4 {
		t.Errorf("weatherstation fields = %v, want explicit 4", byID["climate_weatherstation"])
	}
	if len(byID["climate_basement"]) != len(climate.FieldNames()) {
		t.Errorf("basement fields should fall back to full registry, got %v", byID["climate_basement"])
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
	for _, f := range resp.Fields {
		byName[f.Name] = f.Unit
		if f.DefaultFn != "mean" {
			t.Errorf("%s default_fn = %q, want mean", f.Name, f.DefaultFn)
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
	// Both climate devices present; non-climate winefridge excluded.
	keys := seriesKeys(t, w)
	if len(keys) != 2 {
		t.Fatalf("want 2 climate series, got %d: %v", len(keys), keys)
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
