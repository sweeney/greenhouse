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

// group_by=floor is the grouping the README used to refuse, and group_fn is why
// it can now exist: the API stops asserting that the mean IS the floor and lets
// the caller say which question they are asking.
//
// The two axes are applied in order — fn inside Influx per device, then group_fn
// here across the group's devices — and they do not commute.

// groupDevices spans two floors: floor1 has three sensors across two rooms (so a
// floor combine differs from a room combine), floor2 has one, and one sensor
// declares no floor at all so the UNKNOWN path is always exercised.
func groupDevices() map[string]config.DeviceConfig {
	return map[string]config.DeviceConfig{
		"sensor_e": {
			Class: "environmental_sensor", Floor: "floor1", Room: "floor1.room-a",
			DisplayName: "Sensor E",
		},
		"sensor_f": {
			Class: "environmental_sensor", Floor: "floor1", Room: "floor1.room-a",
			DisplayName: "Sensor F",
		},
		"sensor_g": {
			Class: "environmental_sensor", Floor: "floor1", Room: "floor1.room-b",
			DisplayName: "Sensor G",
		},
		"sensor_h": {
			Class: "environmental_sensor", Floor: "floor2", Room: "floor2.room-a",
			DisplayName: "Sensor H",
		},
		// No declared floor: UNKNOWN, so never grouped under a floor.
		"sensor_x": {
			Class: "environmental_sensor", Room: "floor3.room-a", DisplayName: "Sensor X",
		},
		// Non-climate, on a floor that has climate coverage.
		"plug_a": {
			Class: "continuous_power_device", Floor: "floor1", Room: "floor1.room-a",
			DisplayName: "Plug A",
		},
	}
}

// groupSetup gives each sensor a distinct constant, so mean, min and max over
// floor1 are three recognisably different numbers (20, 10 and 30).
func groupSetup(t *testing.T) (*Server, *influx.FakeQuerier) {
	t.Helper()
	s, q := dataSetup(t)
	s.Config = fakeConfig{devices: groupDevices()}
	values := map[string]float64{
		"sensor_e": 10, "sensor_f": 20, "sensor_g": 30, "sensor_h": 5, "sensor_x": 99,
	}
	q.QueryFunc = func(string) ([]influx.Row, error) {
		var rows []influx.Row
		for id, v := range values {
			rows = append(rows, bucketRows(t, s, id, "today", "1h", v)...)
		}
		return rows, nil
	}
	return s, q
}

// seriesValues decodes key → first-bucket value, plus the echoed group_fn.
func seriesValues(t *testing.T, w *httptest.ResponseRecorder) (map[string]float64, string) {
	t.Helper()
	var resp struct {
		GroupBy string `json:"group_by"`
		GroupFn string `json:"group_fn"`
		Series  []struct {
			Key    string     `json:"key"`
			Room   string     `json:"room"`
			Values []*float64 `json:"values"`
		} `json:"series"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %s: %v", w.Body.String(), err)
	}
	out := map[string]float64{}
	for _, s := range resp.Series {
		if len(s.Values) > 0 && s.Values[0] != nil {
			out[s.Key] = *s.Values[0]
		}
	}
	return out, resp.GroupFn
}

func groupGET(t *testing.T, s *Server, query string) *httptest.ResponseRecorder {
	t.Helper()
	return doGET(t, s, "/series?window=today&interval=1h&"+query)
}

// --- group_by=floor ---

// The headline: a floor is chartable as one line spanning all its rooms.
func TestSeries_GroupByFloor(t *testing.T) {
	s, _ := groupSetup(t)
	w := groupGET(t, s, "group_by=floor")

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	got, _ := seriesValues(t, w)
	if len(got) != 2 {
		t.Fatalf("want one series per floor, got %v", got)
	}
	// floor1 spans two rooms and three sensors: mean(10, 20, 30).
	if got["floor1"] != 20 {
		t.Errorf("floor1 = %v, want 20", got["floor1"])
	}
	if got["floor2"] != 5 {
		t.Errorf("floor2 = %v, want 5", got["floor2"])
	}
}

// A device declaring no floor is UNKNOWN and is omitted, exactly as a room-less
// device is omitted from group_by=room. Settled the same way on both axes.
func TestSeries_GroupByFloor_OmitsUndeclaredFloor(t *testing.T) {
	s, _ := groupSetup(t)
	got, _ := seriesValues(t, groupGET(t, s, "group_by=floor"))

	for key, v := range got {
		if key == "" {
			t.Error(`a series was keyed on "", which is not a valid floor id`)
		}
		if v == 99 {
			t.Errorf("group %q absorbed sensor_x, which declares no floor", key)
		}
	}
}

// Class is applied before grouping, so a plug on a covered floor is never a member.
func TestSeries_GroupByFloor_ExcludesNonClimateDevices(t *testing.T) {
	s, _ := groupSetup(t)
	got, _ := seriesValues(t, groupGET(t, s, "group_by=floor"))

	// mean(10, 20, 30) = 20. A fourth member would move it.
	if got["floor1"] != 20 {
		t.Errorf("floor1 = %v, want 20: a non-climate device leaked into the floor", got["floor1"])
	}
}

// floors= and group_by=floor compose: filter to a storey, chart it as one line.
func TestSeries_GroupByFloor_ComposesWithFloorsFilter(t *testing.T) {
	s, _ := groupSetup(t)
	got, _ := seriesValues(t, groupGET(t, s, "group_by=floor&floors=floor1"))

	if len(got) != 1 || got["floor1"] != 20 {
		t.Errorf("want just floor1 at 20, got %v", got)
	}
}

// A floor-grouped series belongs to no single room, so it must not carry a floor
// id in a field named room.
func TestSeries_GroupByFloor_OmitsRoomField(t *testing.T) {
	s, _ := groupSetup(t)
	w := groupGET(t, s, "group_by=floor")

	var resp struct {
		Series []map[string]any `json:"series"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, ser := range resp.Series {
		if room, ok := ser["room"]; ok {
			t.Errorf("floor series carries room=%v; a floor is not a room", room)
		}
	}
}

// group_by=room is unchanged by any of this.
func TestSeries_GroupByRoom_Unchanged(t *testing.T) {
	s, _ := groupSetup(t)
	got, _ := seriesValues(t, groupGET(t, s, "group_by=room"))

	if got["floor1.room-a"] != 15 {
		t.Errorf("floor1.room-a = %v, want mean(10, 20) = 15", got["floor1.room-a"])
	}
	if got["floor1.room-b"] != 30 {
		t.Errorf("floor1.room-b = %v, want 30", got["floor1.room-b"])
	}
}

// --- group_fn ---

func TestSeries_GroupFn_MinMeanMax(t *testing.T) {
	s, _ := groupSetup(t)
	for _, tc := range []struct {
		fn   string
		want float64
	}{
		{"mean", 20},
		{"min", 10},
		{"max", 30},
	} {
		t.Run(tc.fn, func(t *testing.T) {
			w := groupGET(t, s, "group_by=floor&group_fn="+tc.fn)
			if w.Code != http.StatusOK {
				t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
			}
			got, echoed := seriesValues(t, w)
			if got["floor1"] != tc.want {
				t.Errorf("floor1 = %v, want %v", got["floor1"], tc.want)
			}
			// The response says which question was answered.
			if echoed != tc.fn {
				t.Errorf("group_fn echoed as %q, want %q", echoed, tc.fn)
			}
		})
	}
}

// Omitting group_fn is the old behaviour, byte for byte: mean, echoed as mean.
func TestSeries_GroupFn_DefaultsToMean(t *testing.T) {
	s, _ := groupSetup(t)
	got, echoed := seriesValues(t, groupGET(t, s, "group_by=floor"))

	if got["floor1"] != 20 {
		t.Errorf("floor1 = %v, want the mean 20 by default", got["floor1"])
	}
	if echoed != "mean" {
		t.Errorf("group_fn echoed as %q, want mean", echoed)
	}
}

// group_fn applies to room grouping too — the room case needed a caller-chosen
// combine just as much as the floor case, which is the whole argument for it.
func TestSeries_GroupFn_AppliesToRoomGrouping(t *testing.T) {
	s, _ := groupSetup(t)
	got, _ := seriesValues(t, groupGET(t, s, "group_by=room&group_fn=max"))

	if got["floor1.room-a"] != 20 {
		t.Errorf("floor1.room-a = %v, want max(10, 20) = 20", got["floor1.room-a"])
	}
}

// group_by=device combines nothing, so group_fn is omitted from the response
// rather than advertising a combine that never ran.
func TestSeries_GroupFn_OmittedForGroupByDevice(t *testing.T) {
	s, _ := groupSetup(t)
	w := groupGET(t, s, "group_by=device")

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v, ok := resp["group_fn"]; ok {
		t.Errorf("group_fn = %v present for group_by=device, want omitted", v)
	}
}

// --- rejections ---

// last is valid for fn= but is not a spatial statistic: across members it means
// "whichever sensor reported most recently". Called out by name because it is
// the plausible mistake.
func TestSeries_GroupFn_LastIs400(t *testing.T) {
	s, _ := groupSetup(t)
	w := groupGET(t, s, "group_by=floor&group_fn=last")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "last") {
		t.Errorf("error should name the offending value, got %s", body)
	}
	if !strings.Contains(body, "mean") {
		t.Errorf("error should list the valid combines, got %s", body)
	}
}

// sum is absent on this axis too: climate is non-additive however it is sliced.
func TestSeries_GroupFn_UnknownValuesAre400(t *testing.T) {
	s, _ := groupSetup(t)
	for _, bad := range []string{"sum", "median", "MEAN", "avg", ""} {
		t.Run("group_fn="+bad, func(t *testing.T) {
			w := groupGET(t, s, "group_by=floor&group_fn="+bad)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("group_fn=%q: want 400, got %d: %s", bad, w.Code, w.Body.String())
			}
		})
	}
}

// The identity case. A caller who writes group_fn with group_by=device believes
// an aggregation is happening that is not, so say so rather than ignoring it.
func TestSeries_GroupFn_WithGroupByDeviceIs400(t *testing.T) {
	s, _ := groupSetup(t)
	w := groupGET(t, s, "group_by=device&group_fn=mean")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{"group_fn", "device"} {
		if !strings.Contains(body, want) {
			t.Errorf("error should mention %q, got %s", want, body)
		}
	}
}

// group_by defaults to device, so a bare group_fn is the same client error.
func TestSeries_GroupFn_WithoutGroupByIs400(t *testing.T) {
	s, _ := groupSetup(t)
	w := groupGET(t, s, "group_fn=mean")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 (group_by defaults to device), got %d: %s", w.Code, w.Body.String())
	}
}

// An invalid group_by is still rejected, and the message now offers floor.
func TestSeries_InvalidGroupByNamesFloor(t *testing.T) {
	s, _ := groupSetup(t)
	w := groupGET(t, s, "group_by=house")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "floor") {
		t.Errorf("the valid-values list should now include floor, got %s", w.Body.String())
	}
}

// A circular field cannot be grouped across members whatever the combine: min
// and max of a bearing are as meaningless as the mean.
func TestSeries_GroupFn_CircularRejectedForEveryCombine(t *testing.T) {
	s, _ := dataSetup(t)
	s.Config = fakeConfig{devices: map[string]config.DeviceConfig{
		"probe_a": {Class: "environmental_sensor", Floor: "floor1", Room: "floor1.room-a"},
		"probe_b": {Class: "environmental_sensor", Floor: "floor1", Room: "floor1.room-b"},
	}}
	for _, fn := range []string{"mean", "min", "max"} {
		t.Run(fn, func(t *testing.T) {
			w := groupGET(t, s, "field=wind_dir_deg&group_by=floor&group_fn="+fn)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("group_fn=%s: want 400, got %d: %s", fn, w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "floor1") {
				t.Errorf("error should name the offending floor, got %s", w.Body.String())
			}
		})
	}
}

// The single-device endpoint promises one series, so it groups nothing. group_fn
// there is ignored rather than 400ing, matching how it ignores devices=/rooms=/
// floors=/group_by.
func TestDeviceSeries_IgnoresGroupFn(t *testing.T) {
	s, _ := groupSetup(t)
	w := doGET(t, s, "/devices/sensor_e/series?window=today&interval=1h&group_fn=max")

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
}

// --- shape=rows ---

// shape=rows shares the assembly step, so it gets floor grouping and the combine
// too, and echoes the group_fn in its own metadata.
func TestSeries_GroupByFloor_RowsShape(t *testing.T) {
	s, _ := groupSetup(t)
	w := groupGET(t, s, "group_by=floor&group_fn=max&shape=rows")

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		GroupBy string `json:"group_by"`
		GroupFn string `json:"group_fn"`
		Rows    []struct {
			Key   string   `json:"key"`
			Value *float64 `json:"value"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.GroupBy != "floor" || resp.GroupFn != "max" {
		t.Errorf("rows metadata = %s/%s, want floor/max", resp.GroupBy, resp.GroupFn)
	}
	var sawFloor1 bool
	for _, row := range resp.Rows {
		if row.Key != "floor1" {
			continue
		}
		sawFloor1 = true
		if row.Value == nil || *row.Value != 30 {
			t.Errorf("floor1 row = %v, want the max 30", row.Value)
		}
	}
	if !sawFloor1 {
		t.Error("no floor1 rows")
	}
}

// --- the two axes ---

// fn and group_fn do not commute, and the request carries both independently:
// fn reaches Influx, group_fn stays in Go. Pinned on the flux itself, because a
// group_fn that leaked into the query would be a different question entirely.
func TestSeries_GroupFn_DoesNotLeakIntoTheFluxQuery(t *testing.T) {
	s, q := groupSetup(t)
	q.Queries = nil

	w := groupGET(t, s, "group_by=floor&group_fn=max&fn=min")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(q.Queries) != 1 {
		t.Fatalf("want exactly 1 flux query, got %d", len(q.Queries))
	}
	flux := q.Queries[0]
	// fn=min is the per-device bucket aggregate and belongs in the query.
	if !strings.Contains(flux, "min") {
		t.Errorf("flux should carry fn=min, got %s", flux)
	}
	// The member combine is a Go-side step; the query must not have been rewritten
	// to aggregate across devices.
	if strings.Contains(flux, "group_fn") {
		t.Errorf("group_fn leaked into the flux: %s", flux)
	}
}
