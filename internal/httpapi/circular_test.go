package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sweeney/greenhouse/internal/config"
	"github.com/sweeney/greenhouse/internal/influx"
)

// `fn=` already refuses mean/min/max for wind_dir_deg because arithmetic is
// wrong on a 0–360° axis. Grouping combines readings across a group's members
// with exactly that arithmetic, so the same request has to be refused there —
// otherwise the API guards one axis and quietly breaks the rule on the other.
//
// The API answers with a 400 rather than serving gaps: null means "no reading",
// so a silently-gapped series is indistinguishable from a sensor outage.

// windDevices puts two wind sensors in ONE room and a third in a room of its
// own, so a single fixture exercises both the conflicting and the answerable
// grouping.
func windDevices() map[string]config.DeviceConfig {
	return map[string]config.DeviceConfig{
		"probe_a": {
			Class: "environmental_sensor", Room: "floor1.room-a", Floor: "floor1",
			DisplayName: "Probe A",
		},
		"probe_b": {
			Class: "environmental_sensor", Room: "floor1.room-a", Floor: "floor1",
			DisplayName: "Probe B",
		},
		"outdoor_station": {
			Class: "environmental_sensor", Room: "floor1.room-b", Floor: "floor1",
			DisplayName: "Outdoor Station",
		},
	}
}

// windSetup wires the wind fixture with a querier answering for every device.
func windSetup(t *testing.T) (*Server, *influx.FakeQuerier) {
	t.Helper()
	s, q := dataSetup(t)
	s.Config = fakeConfig{devices: windDevices()}
	q.QueryFunc = func(string) ([]influx.Row, error) {
		var rows []influx.Row
		for _, id := range []string{"probe_a", "probe_b", "outdoor_station"} {
			rows = append(rows, bucketRows(t, s, id, "today", "1h", 350)...)
		}
		return rows, nil
	}
	return s, q
}

// seriesStats decodes the per-series summary stats, keeping them as *float64 so
// a JSON null is distinguishable from a zero.
type seriesStats struct {
	Key    string     `json:"key"`
	Values []*float64 `json:"values"`
	Min    *float64   `json:"min"`
	Max    *float64   `json:"max"`
	Mean   *float64   `json:"mean"`
}

func decodeSeriesStats(t *testing.T, w *httptest.ResponseRecorder) map[string]seriesStats {
	t.Helper()
	var resp struct {
		Series []seriesStats `json:"series"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %s: %v", w.Body.String(), err)
	}
	out := map[string]seriesStats{}
	for _, s := range resp.Series {
		out[s.Key] = s
	}
	return out
}

// The headline case: two wind sensors in one room cannot be grouped, because
// the mean of 350° and 10° is 180° — due South, when both readings say North.
func TestSeries_CircularGroupedAcrossMembersIs400(t *testing.T) {
	s, _ := windSetup(t)
	w := doGET(t, s, "/series?window=today&interval=1h&field=wind_dir_deg&group_by=room")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// The message has to be actionable: which field, which group, and a way out.
	for _, want := range []string{"wind_dir_deg", "floor1.room-a", "group_by=device"} {
		if !strings.Contains(body, want) {
			t.Errorf("error should mention %q, got %s", want, body)
		}
	}
}

// The refusal happens BEFORE Influx is queried. A request that cannot be
// answered should not cost a round trip, and validating after the query would
// mean the error depended on what came back.
func TestSeries_CircularConflictRejectedBeforeQuerying(t *testing.T) {
	s, q := windSetup(t)
	doGET(t, s, "/series?window=today&interval=1h&field=wind_dir_deg&group_by=room")

	if len(q.Queries) != 0 {
		t.Errorf("want no flux query for a rejected request, got %d: %v", len(q.Queries), q.Queries)
	}
}

// group_by=device never combines members, so it charts every bearing. This is
// the escape hatch the 400 names, so it must actually work.
func TestSeries_CircularByDeviceIsAllowed(t *testing.T) {
	s, _ := windSetup(t)
	w := doGET(t, s, "/series?window=today&interval=1h&field=wind_dir_deg&group_by=device")

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	got := decodeSeriesStats(t, w)
	for _, id := range []string{"probe_a", "probe_b", "outdoor_station"} {
		ser, ok := got[id]
		if !ok {
			t.Fatalf("no series for %s, got %v", id, got)
		}
		if len(ser.Values) == 0 || ser.Values[0] == nil || *ser.Values[0] != 350 {
			t.Errorf("%s: want the bearing 350 passed through, got %v", id, ser.Values)
		}
	}
}

// One sensor per room combines nothing, so grouping by room is answerable. The
// fix must not reject requests that were always correct — this is today's fleet.
func TestSeries_CircularGroupedWithSingletonRoomsIs200(t *testing.T) {
	s, _ := dataSetup(t)
	s.Config = fakeConfig{devices: map[string]config.DeviceConfig{
		"probe_a":         {Class: "environmental_sensor", Room: "floor1.room-a"},
		"outdoor_station": {Class: "environmental_sensor", Room: "floor1.room-b"},
	}}
	w := doGET(t, s, "/series?window=today&interval=1h&field=wind_dir_deg&group_by=room")

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
}

// The class allowlist is applied before the conflict check, so a non-climate
// device sharing the room is not a member and cannot trigger a false 400.
func TestSeries_CircularConflictIgnoresNonClimateDevices(t *testing.T) {
	s, _ := dataSetup(t)
	s.Config = fakeConfig{devices: map[string]config.DeviceConfig{
		"probe_a": {Class: "environmental_sensor", Room: "floor1.room-a"},
		"plug_a":  {Class: "continuous_power_device", Room: "floor1.room-a"},
	}}
	w := doGET(t, s, "/series?window=today&interval=1h&field=wind_dir_deg&group_by=room")

	if w.Code != http.StatusOK {
		t.Fatalf("want 200: a plug is not a climate member, got %d: %s", w.Code, w.Body.String())
	}
}

// environment_fields narrowing runs BEFORE the conflict check, so a room whose
// second sensor cannot report wind is a singleton for this field and is
// answerable. Ordering the two the other way round would 400 a request that has
// exactly one candidate reading.
func TestSeries_CircularConflictRunsAfterFieldNarrowing(t *testing.T) {
	s, _ := dataSetup(t)
	s.Config = fakeConfig{devices: map[string]config.DeviceConfig{
		"probe_a": {
			Class: "environmental_sensor", Room: "floor1.room-a",
			EnvironmentFields: []string{"wind_dir_deg"},
		},
		// Declares temperature only: not a candidate for wind_dir_deg at all.
		"alarm_a": {
			Class: "fire_alarm", Room: "floor1.room-a",
			EnvironmentFields: []string{"temperature_c"},
		},
	}}
	w := doGET(t, s, "/series?window=today&interval=1h&field=wind_dir_deg&group_by=room")

	if w.Code != http.StatusOK {
		t.Fatalf("want 200: only one device can report wind here, got %d: %s",
			w.Code, w.Body.String())
	}
}

// Narrowing the request with devices= until each group holds one sensor is the
// other way out, and the error message implies it works.
func TestSeries_CircularConflictClearsWhenNarrowedToOneDevice(t *testing.T) {
	s, _ := windSetup(t)
	w := doGET(t, s,
		"/series?window=today&interval=1h&field=wind_dir_deg&group_by=room&devices=probe_a")

	if w.Code != http.StatusOK {
		t.Fatalf("want 200 once the room holds one selected sensor, got %d: %s",
			w.Code, w.Body.String())
	}
	if got := seriesKeys(t, w); len(got) != 1 || got[0] != "floor1.room-a" {
		t.Errorf("series keys = %v, want the single room", got)
	}
}

// A linear field across the same two co-located sensors is a legitimate mean and
// must be completely untouched by the fix.
func TestSeries_LinearFieldStillGroupsAcrossMembers(t *testing.T) {
	s, q := windSetup(t)
	q.QueryFunc = func(string) ([]influx.Row, error) {
		rows := bucketRows(t, s, "probe_a", "today", "1h", 20)
		rows = append(rows, bucketRows(t, s, "probe_b", "today", "1h", 22)...)
		return rows, nil
	}
	w := doGET(t, s, "/series?window=today&interval=1h&field=temperature_c&group_by=room")

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	got := decodeSeriesStats(t, w)["floor1.room-a"]
	if len(got.Values) == 0 || got.Values[0] == nil || *got.Values[0] != 21 {
		t.Errorf("want the mean 21 across the room's two sensors, got %v", got.Values)
	}
}

// Min/Max/Mean are linear statistics, so they are null for a circular field: a
// legend reading "min 10°, max 350°" describes a 20° spread as though it were
// 340°. The values themselves are still served — refusing to summarise is not
// refusing to chart.
func TestSeries_CircularSummaryStatsAreNull(t *testing.T) {
	s, _ := windSetup(t)
	w := doGET(t, s, "/series?window=today&interval=1h&field=wind_dir_deg&group_by=device")

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	ser := decodeSeriesStats(t, w)["probe_a"]
	for name, v := range map[string]*float64{"min": ser.Min, "max": ser.Max, "mean": ser.Mean} {
		if v != nil {
			t.Errorf("%s = %v, want null for a bearing", name, *v)
		}
	}
	if ser.Values[0] == nil {
		t.Error("values must still carry the bearings themselves")
	}
}

// A linear field keeps its summary stats — the nulling is scoped to circular.
func TestSeries_LinearSummaryStatsSurvive(t *testing.T) {
	s, _ := windSetup(t)
	w := doGET(t, s, "/series?window=today&interval=1h&field=temperature_c&group_by=device")

	ser := decodeSeriesStats(t, w)["probe_a"]
	if ser.Min == nil || ser.Max == nil || ser.Mean == nil {
		t.Errorf("linear summary stats must be real numbers, got %v/%v/%v",
			ser.Min, ser.Max, ser.Mean)
	}
}

// The single-device endpoint charts exactly one device, so it never groups and a
// circular field is always answerable there.
func TestDeviceSeries_CircularIsAllowed(t *testing.T) {
	s, _ := windSetup(t)
	w := doGET(t, s, "/devices/probe_a/series?window=today&interval=1h&field=wind_dir_deg")

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
}

// The circular rule is about grouping, not about fn: fn=last remains the only
// valid aggregation for a bearing, and the pre-existing 400 still fires first.
func TestSeries_CircularStillRejectsLinearFn(t *testing.T) {
	s, _ := windSetup(t)
	w := doGET(t, s, "/series?window=today&interval=1h&field=wind_dir_deg&fn=mean&group_by=device")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "fn") {
		t.Errorf("want the pre-existing fn error, got %s", w.Body.String())
	}
}

// shape=rows shares the assembly step, so the refusal reaches it too rather than
// being a quirk of the columnar encoder.
func TestSeries_CircularConflictAppliesToRowsShape(t *testing.T) {
	s, _ := windSetup(t)
	w := doGET(t, s,
		"/series?window=today&interval=1h&field=wind_dir_deg&group_by=room&shape=rows")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for shape=rows too, got %d: %s", w.Code, w.Body.String())
	}
}
