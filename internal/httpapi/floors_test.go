package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sweeney/greenhouse/internal/config"
	"github.com/sweeney/greenhouse/internal/influx"
)

// The floorplan taxonomy spells a room id "<floor>.<slug>", so a floor is a set of
// rooms. `floors=` selects every climate device on those floors — the coarse filter
// `rooms=` cannot express without the caller enumerating the floorplan itself.

// floorDevices spans three floors, includes a non-climate device on a floor that has
// climate coverage, and keeps one device on the deprecated free-text location so the
// "floor unknown" path is exercised by every test using this fixture.
func floorDevices() map[string]config.DeviceConfig {
	return map[string]config.DeviceConfig{
		"climate_kitchen": {
			Class: "environmental_sensor", Room: "groundfloor.kitchen", DisplayName: "Kitchen",
		},
		"climate_hall": {
			Class: "environmental_sensor", Room: "groundfloor.hall", DisplayName: "Hall",
		},
		"climate_drawingroom": {
			Class: "environmental_sensor", Room: "firstfloor.drawing-room", DisplayName: "Drawing Room",
		},
		"firealarm_utility": {
			Class: "fire_alarm", Room: "secondfloor.utility", DisplayName: "Fire Alarm: Utility",
			EnvironmentFields: []string{"temperature_c"},
		},
		// Non-climate, on a floor that does have climate devices: must never be
		// charted, because class is applied before the floor filter.
		"winefridge": {
			Class: "continuous_power_device", Room: "groundfloor.kitchen", DisplayName: "Wine Fridge",
		},
		// Not yet migrated: no room id, therefore no floor, therefore never
		// selected by floors= — "unknown" is not a floor.
		"climate_basement": {
			Class: "environmental_sensor", Location: "basement", DisplayName: "Basement",
		},
	}
}

// floorSetup wires the floor fixture and a querier that answers for every device.
func floorSetup(t *testing.T) *Server {
	t.Helper()
	s, q := dataSetup(t)
	s.Config = fakeConfig{devices: floorDevices()}
	q.QueryFunc = func(string) ([]influx.Row, error) {
		return bucketRows(t, s, "climate_kitchen", "today", "1h", 20), nil
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
	assertKeys(t, floorKeys(t, s, "floors=groundfloor"), "climate_kitchen", "climate_hall")
}

func TestSeries_FloorsFilter_MultipleFloors(t *testing.T) {
	s := floorSetup(t)
	assertKeys(t, floorKeys(t, s, "floors=groundfloor,firstfloor"),
		"climate_kitchen", "climate_hall", "climate_drawingroom")
}

// Class is applied before the floor filter: the wine fridge sits in
// groundfloor.kitchen but is not a climate device, so it is never a candidate.
func TestSeries_FloorsFilter_ExcludesNonClimateDevices(t *testing.T) {
	s := floorSetup(t)
	for _, k := range floorKeys(t, s, "floors=groundfloor") {
		if k == "winefridge" {
			t.Fatal("non-climate device leaked through floors=")
		}
	}
}

// A fire alarm is a first-class climate device, so it is selectable by floor like
// any other — which matters because it is the only climate coverage some rooms have.
func TestSeries_FloorsFilter_IncludesFireAlarm(t *testing.T) {
	s := floorSetup(t)
	assertKeys(t, floorKeys(t, s, "floors=secondfloor"), "firealarm_utility")
}

// A device with no room id has an unknown floor, and unknown is not a floor: it is
// never swept in by a floors= request. Selecting it needs devices= or a migration.
func TestSeries_FloorsFilter_NeverSelectsUnknownFloor(t *testing.T) {
	s := floorSetup(t)
	for _, k := range floorKeys(t, s, "floors=groundfloor,firstfloor,secondfloor") {
		if k == "climate_basement" {
			t.Fatal("a device with no room id was selected by floors=; unknown is not a floor")
		}
	}
}

// Unfiltered behaviour is untouched — including the unmigrated device, which only
// floors= excludes.
func TestSeries_NoFloorsFilterChartsEverything(t *testing.T) {
	s := floorSetup(t)
	assertKeys(t, floorKeys(t, s, ""),
		"climate_kitchen", "climate_hall", "climate_drawingroom",
		"firealarm_utility", "climate_basement")
}

// An unknown floor is a 400, mirroring rooms=: a floor with no climate device does
// not exist as far as the climate API is concerned, so answering with an empty
// series would hide the client's typo behind a plausible-looking empty chart.
func TestSeries_FloorsFilter_UnknownFloorIs400(t *testing.T) {
	s := floorSetup(t)
	for _, f := range []string{"thirdfloor", "groundfloor.kitchen", "GroundFloor", "basement"} {
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
	w := doGET(t, s, "/series?window=today&interval=1h&floors=groundfloor,nosuchfloor")
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
	assertKeys(t, floorKeys(t, s, "floors=%20groundfloor%20,%20firstfloor"),
		"climate_kitchen", "climate_hall", "climate_drawingroom")
}

// floors= composes as AND with rooms=: the intersection, not the union.
func TestSeries_FloorsAndRoomsCompose(t *testing.T) {
	s := floorSetup(t)
	assertKeys(t, floorKeys(t, s, "floors=groundfloor&rooms=groundfloor.kitchen"), "climate_kitchen")
}

// floors= composes as AND with devices= too.
func TestSeries_FloorsAndDevicesCompose(t *testing.T) {
	s := floorSetup(t)
	assertKeys(t, floorKeys(t, s, "floors=groundfloor&devices=climate_hall"), "climate_hall")
}

// All three filters at once still intersect.
func TestSeries_FloorsRoomsAndDevicesCompose(t *testing.T) {
	s := floorSetup(t)
	assertKeys(t, floorKeys(t, s,
		"floors=groundfloor&rooms=groundfloor.kitchen,groundfloor.hall&devices=climate_kitchen"),
		"climate_kitchen")
}

// A valid-but-disjoint intersection is an empty 200, not a 400: every named floor,
// room and device exists, so there is no client error to report — just no overlap.
func TestSeries_FloorsDisjointFromRoomsIsEmpty200(t *testing.T) {
	s := floorSetup(t)
	if got := floorKeys(t, s, "floors=firstfloor&rooms=groundfloor.kitchen"); len(got) != 0 {
		t.Errorf("want an empty series list, got %v", got)
	}
}

// Duplicates in the CSV are a set, not a multiplier: no duplicate series.
func TestSeries_FloorsFilter_DuplicatesAreASet(t *testing.T) {
	s := floorSetup(t)
	assertKeys(t, floorKeys(t, s, "floors=groundfloor,groundfloor"),
		"climate_kitchen", "climate_hall")
}

// floors= narrows group_by=room the same way it narrows group_by=device: the
// filter chooses the devices, the grouping only decides how they are keyed.
func TestSeries_FloorsFilter_WithGroupByRoom(t *testing.T) {
	s := floorSetup(t)
	assertKeys(t, floorKeys(t, s, "floors=groundfloor&group_by=room"),
		"groundfloor.kitchen", "groundfloor.hall")
}

// The single-device endpoints take no filters; floors= there is ignored rather
// than silently changing which device is charted.
func TestDeviceSeries_IgnoresFloorsFilter(t *testing.T) {
	s := floorSetup(t)
	w := doGET(t, s, "/devices/climate_kitchen/series?window=today&interval=1h&floors=firstfloor")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
}
