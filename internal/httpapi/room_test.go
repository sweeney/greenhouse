package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sweeney/greenhouse/internal/config"
	"github.com/sweeney/greenhouse/internal/influx"
)

// The floorplan migration renames the read-side vocabulary: `location` meant both a
// geographic site and a room, so rooms are now `room`. The deprecated spelling has been
// removed, and what remains here pins the room behaviour itself — including that the
// removed parameter is rejected rather than ignored.

// roomDevices mirrors testDevices but with the published room ids the devices namespace
// will carry after the migration.
func roomDevices() map[string]config.DeviceConfig {
	return map[string]config.DeviceConfig{
		"sensor_a": {
			Class: "environmental_sensor", Room: "floor1.room-a", DisplayName: "Sensor A",
		},
		"outdoor_station": {
			Class: "environmental_sensor", Room: "floor1.room-b", DisplayName: "Outdoor Station",
		},
		"alarm_a": {
			Class: "fire_alarm", Room: "floor1.room-c", DisplayName: "Alarm A",
			EnvironmentFields: []string{"temperature_c"},
		},
		"plug_a": {
			Class: "continuous_power_device", Room: "floor2.room-a", DisplayName: "Plug A",
		},
	}
}

func TestSeries_GroupByRoomKeysOnRoomIDs(t *testing.T) {
	s, q := dataSetup(t)
	s.Config = fakeConfig{devices: roomDevices()}
	q.QueryFunc = func(string) ([]influx.Row, error) {
		return bucketRows(t, s, "sensor_a", "today", "1h", 20), nil
	}

	w := doGET(t, s, "/series?window=today&interval=1h&group_by=room")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		GroupBy string `json:"group_by"`
		Series  []struct {
			Key string `json:"key"`
		} `json:"series"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.GroupBy != "room" {
		t.Errorf("group_by = %q, want %q", resp.GroupBy, "room")
	}
	found := false
	for _, ser := range resp.Series {
		if ser.Key == "floor1.room-a" {
			found = true
		}
	}
	if !found {
		t.Errorf("no series keyed on the room id: %+v", resp.Series)
	}
}

func TestUnknownRoomIsA400(t *testing.T) {
	s, _ := dataSetup(t)
	s.Config = fakeConfig{devices: roomDevices()}

	w := doGET(t, s, "/series?window=today&interval=1h&rooms=floor1.room-z")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "rooms") {
		t.Errorf("the error must name the parameter the caller used: %s", w.Body.String())
	}
}

// A device namespace that still carries `location` must keep working untouched: the
// namespace and the consumers migrate independently.
func TestLegacyLocationOnlyConfigStillGroups(t *testing.T) {
	s, q := dataSetup(t)
	q.QueryFunc = func(string) ([]influx.Row, error) {
		return bucketRows(t, s, "sensor_a", "today", "1h", 20), nil
	}

	w := doGET(t, s, "/series?window=today&interval=1h&group_by=room")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"key":"area-a"`) {
		t.Errorf("a location-only namespace must still group: %s", w.Body.String())
	}
}

// An empty `rooms=` means no room filter, not a filter that matches nothing. A UI that
// always emits the parameter and empties it when the user clears the filter must get
// every device back.
func TestEmptyRoomsParamMeansNoFilter(t *testing.T) {
	s, q := dataSetup(t)
	s.Config = fakeConfig{devices: roomDevices()}
	q.QueryFunc = func(string) ([]influx.Row, error) {
		return bucketRows(t, s, "sensor_a", "today", "1h", 20), nil
	}

	all := doGET(t, s, "/series?window=today&interval=1h")
	got := doGET(t, s, "/series?window=today&interval=1h&rooms=")

	if got.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", got.Code, got.Body.String())
	}
	if got.Body.String() != all.Body.String() {
		t.Error("an empty rooms= filtered the response instead of meaning no filter")
	}
}

// `locations=` was removed, and an unrecognised query parameter is silently ignored —
// so an un-migrated client asking for one room would get every device instead, a chart
// labelled with one room showing the whole house, with nothing to explain it.
//
// Its sibling spelling already fails loudly: group_by=location 400s. Two opposite
// failure modes for the same removal in the same release, and the silent one produces
// plausible wrong data rather than an error a client can act on.
func TestRemovedLocationsParamIsRejectedNotIgnored(t *testing.T) {
	s, q := dataSetup(t)
	s.Config = fakeConfig{devices: roomDevices()}
	q.QueryFunc = func(string) ([]influx.Row, error) {
		return bucketRows(t, s, "sensor_a", "today", "1h", 20), nil
	}

	w := doGET(t, s, "/series?window=today&interval=1h&locations=floor1.room-c")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "rooms") {
		t.Errorf("the error must name the replacement: %s", w.Body.String())
	}
}

// The catalog passes through the floor the namespace declares for each device,
// and reports empty when it declares none. It never reads a floor out of the room
// id: the floorplan publishes both properties, and greenhouse is not the place
// that decides how a room id is spelled.
func TestDevices_FloorComesFromTheNamespace(t *testing.T) {
	s, _ := dataSetup(t)
	s.Config = fakeConfig{devices: map[string]config.DeviceConfig{
		"sensor_e": {
			Class: "environmental_sensor", Floor: "floor1", Room: "floor1.room-a",
			DisplayName: "Sensor E",
		},
		"sensor_g": {
			Class: "environmental_sensor", Floor: "floor2", Room: "floor2.room-a",
			DisplayName: "Sensor G",
		},
		// Declares a room but no floor: unknown, reported as empty.
		"sensor_f": {
			Class: "environmental_sensor", Room: "floor2.room-b", DisplayName: "Sensor F",
		},
		// Declares neither.
		"sensor_a": {
			Class: "environmental_sensor", Location: "area-a", DisplayName: "Sensor A",
		},
	}}

	want := map[string]string{
		"sensor_e": "floor1",
		"sensor_g": "floor2",
		"sensor_f": "",
		"sensor_a": "",
	}
	got := map[string]string{}
	for _, d := range getCatalog(t, s).Devices {
		got[d.ID] = d.Floor
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s: floor = %q, want %q", id, got[id], w)
		}
	}
}
