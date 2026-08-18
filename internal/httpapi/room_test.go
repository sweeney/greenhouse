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
		"climate_basement": {
			Class: "environmental_sensor", Room: "basement.hallway", DisplayName: "Basement",
		},
		"climate_weatherstation": {
			Class: "environmental_sensor", Room: "basement.garden", DisplayName: "Weather Station",
		},
		"firealarm_utility": {
			Class: "fire_alarm", Room: "basement.utility", DisplayName: "Fire Alarm: Utility",
			EnvironmentFields: []string{"temperature_c"},
		},
		"winefridge": {
			Class: "continuous_power_device", Room: "groundfloor.kitchen", DisplayName: "Wine Fridge",
		},
	}
}

func TestSeries_GroupByRoomKeysOnRoomIDs(t *testing.T) {
	s, q := dataSetup(t)
	s.Config = fakeConfig{devices: roomDevices()}
	q.QueryFunc = func(string) ([]influx.Row, error) {
		return bucketRows(t, s, "climate_basement", "today", "1h", 20), nil
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
		if ser.Key == "basement.hallway" {
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

	w := doGET(t, s, "/series?window=today&interval=1h&rooms=basement.attic")
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
		return bucketRows(t, s, "climate_basement", "today", "1h", 20), nil
	}

	w := doGET(t, s, "/series?window=today&interval=1h&group_by=room")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"key":"basement"`) {
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
		return bucketRows(t, s, "climate_basement", "today", "1h", 20), nil
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
		return bucketRows(t, s, "climate_basement", "today", "1h", 20), nil
	}

	w := doGET(t, s, "/series?window=today&interval=1h&locations=basement.utility")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "rooms") {
		t.Errorf("the error must name the replacement: %s", w.Body.String())
	}
}

// Once the namespace publishes room ids, the catalog reports the floor derived
// from each one. It is derived, never configured, so it cannot disagree with the
// room it came from.
func TestDevices_FloorDerivedFromRoomID(t *testing.T) {
	s, _ := dataSetup(t)
	s.Config = fakeConfig{devices: map[string]config.DeviceConfig{
		"climate_kitchen": {
			Class: "environmental_sensor", Room: "groundfloor.kitchen", DisplayName: "Kitchen",
		},
		"climate_drawingroom": {
			Class: "environmental_sensor", Room: "firstfloor.drawing-room", DisplayName: "Drawing Room",
		},
		// Not yet migrated: no room id, so no floor to report.
		"climate_basement": {
			Class: "environmental_sensor", Location: "basement", DisplayName: "Basement",
		},
	}}

	want := map[string]string{
		"climate_kitchen":     "groundfloor",
		"climate_drawingroom": "firstfloor",
		"climate_basement":    "",
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
