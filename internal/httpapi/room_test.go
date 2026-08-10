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
// geographic site and a room, so rooms are now `room`. `location` and `locations=` stay
// as deprecated aliases for one release so consumers migrate independently — the desktop
// client ships through an app store review and is the slowest lane.
//
// The deprecated `location` spelling has now been removed (step 11), so what remains
// here pins the room behaviour itself.

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

// The fallback keyed on the parsed list being empty rather than the parameter being
// absent, so `rooms=` present-but-empty silently re-enabled the deprecated filter —
// contradicting both the README and the OpenAPI description, which say `locations` is
// ignored when `rooms` is present. A UI that always emits `rooms=` and empties it when
// the user clears the filter would get a filtered response where it expects all devices.
func TestEmptyRoomsParamDoesNotReEnableTheDeprecatedFilter(t *testing.T) {
	s, q := dataSetup(t)
	s.Config = fakeConfig{devices: roomDevices()}
	q.QueryFunc = func(string) ([]influx.Row, error) {
		return bucketRows(t, s, "climate_basement", "today", "1h", 20), nil
	}

	all := doGET(t, s, "/series?window=today&interval=1h")
	got := doGET(t, s, "/series?window=today&interval=1h&rooms=&locations=basement.hallway")

	if got.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", got.Code, got.Body.String())
	}
	if got.Body.String() != all.Body.String() {
		t.Error("an empty rooms= applied the deprecated locations= filter")
	}
}
