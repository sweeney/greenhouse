package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sweeney/greenhouse/internal/config"
	"github.com/sweeney/greenhouse/internal/influx"
)

// A floor is a first-class property of a device in the devices namespace, and
// `floors=` selects on it — the coarse filter `rooms=` cannot express without the
// caller enumerating the floorplan itself.

// floorDevices spans three floors, includes a non-climate device on a floor that has
// climate coverage, and keeps one device whose namespace entry declares no floor, so
// the "floor unknown" path is exercised by every test using this fixture.
func floorDevices() map[string]config.DeviceConfig {
	return map[string]config.DeviceConfig{
		"sensor_e": {
			Class: "environmental_sensor", Floor: "floor1", Room: "floor1.room-a",
			DisplayName: "Sensor E",
		},
		"sensor_f": {
			Class: "environmental_sensor", Floor: "floor1", Room: "floor1.room-b",
			DisplayName: "Sensor F",
		},
		"sensor_g": {
			Class: "environmental_sensor", Floor: "floor2", Room: "floor2.room-a",
			DisplayName: "Sensor G",
		},
		"alarm_a": {
			Class: "fire_alarm", Floor: "floor3", Room: "floor3.room-a",
			DisplayName: "Alarm A", EnvironmentFields: []string{"temperature_c"},
		},
		// Non-climate, on a floor that does have climate devices: must never be
		// charted, because class is applied before the floor filter.
		"plug_a": {
			Class: "continuous_power_device", Floor: "floor1", Room: "floor1.room-a",
			DisplayName: "Plug A",
		},
		// Namespace declares no floor for this one: unknown, and unknown is not a
		// floor, so floors= can never select it.
		"sensor_a": {
			Class: "environmental_sensor", Location: "area-a", DisplayName: "Sensor A",
		},
	}
}

// floorSetup wires the floor fixture and a querier that answers for every device.
func floorSetup(t *testing.T) *Server {
	t.Helper()
	s, q := dataSetup(t)
	s.Config = fakeConfig{devices: floorDevices()}
	q.QueryFunc = func(string) ([]influx.Row, error) {
		return bucketRows(t, s, "sensor_e", "today", "1h", 20), nil
	}
	return s
}

// floorKeys issues a /series request and returns its series keys, failing on a
// non-200. Keys are device ids under the default group_by=device.
func floorKeys(t *testing.T, s *Server, query string) []string {
	t.Helper()
	w := doGET(t, s, "/series?window=today&interval=1h&"+query)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /series?%s: want 200, got %d: %s", query, w.Code, w.Body.String())
	}
	return seriesKeys(t, w)
}

func assertKeys(t *testing.T, got []string, want ...string) {
	t.Helper()
	gotSet := map[string]bool{}
	for _, k := range got {
		gotSet[k] = true
	}
	if len(got) != len(want) {
		t.Fatalf("series keys = %v, want %v", got, want)
	}
	for _, w := range want {
		if !gotSet[w] {
			t.Errorf("series keys = %v, missing %q", got, w)
		}
	}
}

func TestSeries_FloorsFilter_SelectsEveryRoomOnTheFloor(t *testing.T) {
	s := floorSetup(t)
	assertKeys(t, floorKeys(t, s, "floors=floor1"), "sensor_e", "sensor_f")
}

func TestSeries_FloorsFilter_MultipleFloors(t *testing.T) {
	s := floorSetup(t)
	assertKeys(t, floorKeys(t, s, "floors=floor1,floor2"), "sensor_e", "sensor_f", "sensor_g")
}

// Class is applied before the floor filter: plug_a sits on floor1 but is not a
// climate device, so it is never a candidate.
func TestSeries_FloorsFilter_ExcludesNonClimateDevices(t *testing.T) {
	s := floorSetup(t)
	for _, k := range floorKeys(t, s, "floors=floor1") {
		if k == "plug_a" {
			t.Fatal("non-climate device leaked through floors=")
		}
	}
}

// A fire alarm is a first-class climate device, so it is selectable by floor like
// any other — which matters because it is the only climate coverage some rooms have.
func TestSeries_FloorsFilter_IncludesFireAlarm(t *testing.T) {
	s := floorSetup(t)
	assertKeys(t, floorKeys(t, s, "floors=floor3"), "alarm_a")
}

// A device whose namespace entry declares no floor has an unknown floor, and unknown
// is not a floor: it is never swept in by a floors= request, whatever floors are
// named. Selecting it needs devices=, or a floor in its namespace entry.
func TestSeries_FloorsFilter_NeverSelectsUnknownFloor(t *testing.T) {
	s := floorSetup(t)
	for _, k := range floorKeys(t, s, "floors=floor1,floor2,floor3") {
		if k == "sensor_a" {
			t.Fatal("a device with no declared floor was selected by floors=; unknown is not a floor")
		}
	}
}

// Unfiltered behaviour is untouched — including the device with no declared floor,
// which only floors= excludes.
func TestSeries_NoFloorsFilterChartsEverything(t *testing.T) {
	s := floorSetup(t)
	assertKeys(t, floorKeys(t, s, ""),
		"sensor_e", "sensor_f", "sensor_g", "alarm_a", "sensor_a")
}

// An unknown floor is a 400, mirroring rooms=: a floor with no climate device does
// not exist as far as the climate API is concerned, so answering with an empty
// series would hide the client's typo behind a plausible-looking empty chart.
func TestSeries_FloorsFilter_UnknownFloorIs400(t *testing.T) {
	s := floorSetup(t)
	// In order: a floor nobody is on, a room id passed as a floor, a case
	// variant, and a legacy free-text location.
	for _, f := range []string{"floor9", "floor1.room-a", "Floor1", "area-a"} {
		t.Run(f, func(t *testing.T) {
			w := doGET(t, s, "/series?window=today&interval=1h&floors="+f)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("floors=%s: want 400, got %d: %s", f, w.Code, w.Body.String())
			}
			if body := w.Body.String(); !strings.Contains(body, "floors") || !strings.Contains(body, f) {
				t.Errorf("error should name the parameter and the offending value, got %s", body)
			}
		})
	}
}

// One bad floor invalidates the whole request rather than being quietly dropped —
// a partially-honoured filter charts more than the client asked for.
func TestSeries_FloorsFilter_OneUnknownFloorRejectsTheRequest(t *testing.T) {
	s := floorSetup(t)
	w := doGET(t, s, "/series?window=today&interval=1h&floors=floor1,nosuchfloor")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
}

// Empty and whitespace-only CSV entries are dropped, so floors= with no usable
// value is "no filter" rather than an error or an empty chart.
func TestSeries_FloorsFilter_EmptyValueIsNoFilter(t *testing.T) {
	s := floorSetup(t)
	for _, q := range []string{"floors=", "floors=,", "floors=%20"} {
		t.Run(q, func(t *testing.T) {
			got := floorKeys(t, s, q)
			if len(got) != 5 {
				t.Errorf("%s should behave as no filter (5 climate devices), got %v", q, got)
			}
		})
	}
}

// Whitespace around entries is trimmed, matching devices= and rooms=.
func TestSeries_FloorsFilter_TrimsWhitespace(t *testing.T) {
	s := floorSetup(t)
	assertKeys(t, floorKeys(t, s, "floors=%20floor1%20,%20floor2"),
		"sensor_e", "sensor_f", "sensor_g")
}

// floors= composes as AND with rooms=: the intersection, not the union.
func TestSeries_FloorsAndRoomsCompose(t *testing.T) {
	s := floorSetup(t)
	assertKeys(t, floorKeys(t, s, "floors=floor1&rooms=floor1.room-a"), "sensor_e")
}

// floors= composes as AND with devices= too.
func TestSeries_FloorsAndDevicesCompose(t *testing.T) {
	s := floorSetup(t)
	assertKeys(t, floorKeys(t, s, "floors=floor1&devices=sensor_f"), "sensor_f")
}

// All three filters at once still intersect.
func TestSeries_FloorsRoomsAndDevicesCompose(t *testing.T) {
	s := floorSetup(t)
	assertKeys(t, floorKeys(t, s,
		"floors=floor1&rooms=floor1.room-a,floor1.room-b&devices=sensor_e"), "sensor_e")
}

// A valid-but-disjoint intersection is an empty 200, not a 400: every named floor,
// room and device exists, so there is no client error to report — just no overlap.
func TestSeries_FloorsDisjointFromRoomsIsEmpty200(t *testing.T) {
	s := floorSetup(t)
	if got := floorKeys(t, s, "floors=floor2&rooms=floor1.room-a"); len(got) != 0 {
		t.Errorf("want an empty series list, got %v", got)
	}
}

// Duplicates in the CSV are a set, not a multiplier: no duplicate series.
func TestSeries_FloorsFilter_DuplicatesAreASet(t *testing.T) {
	s := floorSetup(t)
	assertKeys(t, floorKeys(t, s, "floors=floor1,floor1"), "sensor_e", "sensor_f")
}

// floors= narrows group_by=room the same way it narrows group_by=device: the
// filter chooses the devices, the grouping only decides how they are keyed.
func TestSeries_FloorsFilter_WithGroupByRoom(t *testing.T) {
	s := floorSetup(t)
	assertKeys(t, floorKeys(t, s, "floors=floor1&group_by=room"),
		"floor1.room-a", "floor1.room-b")
}

// A device's floor is whatever the namespace declares, NOT whatever its room id
// happens to start with. Nothing in greenhouse re-derives it, so a device filed
// under a floor its room id does not name is still charted by the declared floor —
// the floorplan owns that fact, and disagreeing with it would be greenhouse
// inventing a second answer.
func TestSeries_FloorsFilter_TrustsTheDeclaredFloorOverTheRoomID(t *testing.T) {
	s, q := dataSetup(t)
	s.Config = fakeConfig{devices: map[string]config.DeviceConfig{
		"sensor_e": {
			Class: "environmental_sensor", Floor: "floor3", Room: "floor1.room-a",
			DisplayName: "Sensor E",
		},
	}}
	q.QueryFunc = func(string) ([]influx.Row, error) {
		return bucketRows(t, s, "sensor_e", "today", "1h", 20), nil
	}

	assertKeys(t, floorKeys(t, s, "floors=floor3"), "sensor_e")

	// And the room id's leading segment is NOT a floor here, precisely because
	// nothing derives one from it.
	w := doGET(t, s, "/series?window=today&interval=1h&floors=floor1")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("floors=floor1: want 400 (no device declares it), got %d: %s", w.Code, w.Body.String())
	}
}

// The single-device endpoints take no filters; floors= there is ignored rather
// than silently changing which device is charted.
func TestDeviceSeries_IgnoresFloorsFilter(t *testing.T) {
	s := floorSetup(t)
	w := doGET(t, s, "/devices/sensor_e/series?window=today&interval=1h&floors=floor2")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
}

// A floor and a room are independent properties, so a device may declare one
// without the other (see config.TestDeviceConfig_FloorWithoutRoom). floors=
// selects such a device, but group_by=room keys on rooms and it has none — so it
// is absent from that view rather than keyed on "". Pinned because floors= is how
// a caller asks for a whole storey, and this is the one way that view is short.
func TestSeries_FloorsFilter_GroupByRoomOmitsARoomlessDevice(t *testing.T) {
	s, q := dataSetup(t)
	s.Config = fakeConfig{devices: map[string]config.DeviceConfig{
		"sensor_e": {Class: "environmental_sensor", Floor: "floor1", DisplayName: "Sensor E"},
		"sensor_f": {
			Class: "environmental_sensor", Floor: "floor1", Room: "floor1.room-a",
			DisplayName: "Sensor F",
		},
	}}
	q.QueryFunc = func(string) ([]influx.Row, error) {
		return bucketRows(t, s, "sensor_e", "today", "1h", 20), nil
	}

	// The floor filter itself selects both: floors= reads Floor, not Room.
	assertKeys(t, floorKeys(t, s, "floors=floor1"), "sensor_e", "sensor_f")

	// Grouping by room drops the room-less one, and never invents a "" key.
	assertKeys(t, floorKeys(t, s, "floors=floor1&group_by=room"), "floor1.room-a")
}
