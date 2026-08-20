package httpapi

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sweeney/greenhouse/internal/influx"
)

// End-to-end cover for /rooms and the labels it feeds: a real listener, the real
// router, real CORS and auth middleware, a signed ES256 service token, and a
// fake Influx underneath.
//
// It exists because the value here is a JOURNEY across endpoints — read the
// rooms, exclude by category, chart the rest, render a legend — and because the
// catalog and the filter must agree. A unit test can pin each; only the real
// stack shows a consumer the same answer twice.

func roomsIntegrationServer(t *testing.T) (base, token string, q *influx.FakeQuerier) {
	t.Helper()

	s, q := dataSetup(t)
	s.Config = fakeConfig{devices: roomCatalogDevices()}
	s.Floorplan = fakeFloors{floors: roomCatalogFloors(), rooms: roomCatalogRecords()}
	s.PublicURL = "https://greenhouse.swee.net"

	priv := genTestKey(t)
	const kid = "roomskey"
	id := fakeJWKSServer(t, &priv.PublicKey, kid)
	s.IdentityURL = id.URL

	q.QueryFunc = func(string) ([]influx.Row, error) {
		var rows []influx.Row
		for _, dev := range []string{"sensor_e", "sensor_f", "probe_a", "sensor_g", "alarm_a", "sensor_x"} {
			rows = append(rows, bucketRows(t, s, dev, "today", "1h", 20)...)
		}
		return rows, nil
	}

	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)

	return srv.URL, serviceToken(t, priv, kid, id.URL, time.Now().Add(time.Hour)), q
}

type roomsBody struct {
	Rooms []roomRow `json:"rooms"`
}

// The journey the endpoint exists for, and the three client-side derivations it
// removes: read the rooms, drop the plant room BY CATEGORY rather than by
// matching its slug, chart the rest, and render a legend from the labels the API
// supplies rather than from a name transform.
func TestIntegration_RoomCatalogReplacesClientDerivations(t *testing.T) {
	base, token, _ := roomsIntegrationServer(t)

	// Not public: the catalog needs a token like every other data route.
	get(t, base, "", "/rooms", http.StatusUnauthorized, nil)

	var body roomsBody
	get(t, base, token, "/rooms", http.StatusOK, &body)
	if len(body.Rooms) == 0 {
		t.Fatal("no rooms listed")
	}

	// 1. Purpose comes from the API, not from the id's slug.
	var living []string
	var plant []string
	for _, r := range body.Rooms {
		if r.Category == "plant" {
			plant = append(plant, r.ID)
			continue
		}
		living = append(living, r.ID)
	}
	if len(plant) != 1 || plant[0] != "floor1.room-c" {
		t.Fatalf("plant rooms = %v, want exactly floor1.room-c by category", plant)
	}

	// 2. The remaining rooms chart, and the filter accepts every id the catalog
	//    published — so the exclusion is expressible as a request.
	slices.Sort(living)
	var resp struct {
		Series []struct {
			Key   string `json:"key"`
			Label string `json:"label"`
		} `json:"series"`
	}
	get(t, base, token,
		"/series?window=today&interval=1h&group_by=room&rooms="+strings.Join(living, ","),
		http.StatusOK, &resp)

	if len(resp.Series) != len(living) {
		t.Fatalf("charted %d series, want the %d living-space rooms", len(resp.Series), len(living))
	}

	// 3. Legend labels arrive named, so nothing title-cases an id.
	byKey := map[string]string{}
	for _, ser := range resp.Series {
		byKey[ser.Key] = ser.Label
		if ser.Key == "floor1.room-c" {
			t.Error("the plant room was charted despite being excluded by category")
		}
	}
	if byKey["floor1.room-a"] != "Room A" {
		t.Errorf("legend label = %q, want the floorplan's name", byKey["floor1.room-a"])
	}
	// And the key is still the id, so identity matching is unaffected.
	if _, ok := byKey["floor1.room-a"]; !ok {
		t.Error("series key is no longer the room id")
	}
}

// The catalog and the filter must agree in BOTH directions: everything /rooms
// lists is chartable, and every room /devices implies is listed. A gap either way
// means a client discovering rooms from one endpoint and being refused by
// another.
func TestIntegration_RoomsAgreesWithTheDeviceCatalog(t *testing.T) {
	base, token, _ := roomsIntegrationServer(t)

	var catalog struct {
		Devices []struct {
			ID   string `json:"id"`
			Room string `json:"room"`
		} `json:"devices"`
	}
	get(t, base, token, "/devices", http.StatusOK, &catalog)

	fromDevices := map[string]int{}
	for _, d := range catalog.Devices {
		if d.Room != "" {
			fromDevices[d.Room]++
		}
	}

	var body roomsBody
	get(t, base, token, "/rooms", http.StatusOK, &body)

	fromRooms := map[string]int{}
	for _, r := range body.Rooms {
		fromRooms[r.ID] = r.DeviceCount
	}

	if len(fromDevices) != len(fromRooms) {
		t.Fatalf("/devices implies rooms %v, /rooms lists %v", fromDevices, fromRooms)
	}
	for id, want := range fromDevices {
		got, ok := fromRooms[id]
		if !ok {
			t.Errorf("/devices puts devices in %q but /rooms omits it", id)
			continue
		}
		if got != want {
			t.Errorf("%s device_count = %d, but /devices shows %d devices", id, got, want)
		}
	}

	// And every listed room charts.
	for _, r := range body.Rooms {
		t.Run("chart "+r.ID, func(t *testing.T) {
			get(t, base, token, "/series?window=today&interval=1h&rooms="+r.ID,
				http.StatusOK, nil)
		})
	}
}

// /rooms must never advertise a value /series rejects — the property the whole
// listing rule exists to guarantee.
func TestIntegration_RoomsNeverAdvertisesARejectedValue(t *testing.T) {
	base, token, _ := roomsIntegrationServer(t)

	var body roomsBody
	get(t, base, token, "/rooms", http.StatusOK, &body)

	listed := map[string]bool{}
	for _, r := range body.Rooms {
		listed[r.ID] = true
	}

	// floor9.room-a has a floorplan record but no climate device; floor2.room-z
	// holds only a plug. Both are rejected by rooms=, so both must be absent.
	for _, id := range []string{"floor9.room-a", "floor2.room-z"} {
		if listed[id] {
			t.Errorf("/rooms advertises %q", id)
		}
		var errBody map[string]any
		get(t, base, token, "/series?window=today&interval=1h&rooms="+id,
			http.StatusBadRequest, &errBody)
	}
}

// The two catalogs compose: a client joins /rooms to /floors on the room's floor
// id to build an unambiguous "Upper Floor — Room A" label, which it needs because
// room names are NOT unique.
func TestIntegration_RoomsAndFloorsComposeIntoAnUnambiguousLabel(t *testing.T) {
	base, token, _ := roomsIntegrationServer(t)

	var rooms roomsBody
	get(t, base, token, "/rooms", http.StatusOK, &rooms)

	var floors struct {
		Floors []floorRow `json:"floors"`
	}
	get(t, base, token, "/floors", http.StatusOK, &floors)

	floorName := map[string]string{}
	for _, f := range floors.Floors {
		floorName[f.ID] = f.Name
	}

	// Two rooms share the name "Room A"; composing with the floor separates them.
	labels := map[string]string{}
	for _, r := range rooms.Rooms {
		if r.Name == "" {
			continue
		}
		if fn := floorName[r.Floor]; fn != "" {
			labels[r.ID] = fn + " — " + r.Name
		} else {
			labels[r.ID] = r.Name
		}
	}

	if labels["floor1.room-a"] != "Lower Floor — Room A" {
		t.Errorf("composed label = %q, want Lower Floor — Room A", labels["floor1.room-a"])
	}
	if labels["floor2.room-a"] != "Upper Floor — Room A" {
		t.Errorf("composed label = %q, want Upper Floor — Room A", labels["floor2.room-a"])
	}
	if labels["floor1.room-a"] == labels["floor2.room-a"] {
		t.Error("the two same-named rooms produced the same label; the join failed")
	}
}

// With no floorplan namespace configured — the default for an instance that
// never sets one — /rooms still lists every climate room and series stay labelled
// by id. Floorplan detail is presentation and must never gate the climate API.
func TestIntegration_NoFloorplanStillServesRoomsAndIDLabels(t *testing.T) {
	s, q := dataSetup(t)
	s.Config = fakeConfig{devices: roomCatalogDevices()}
	s.Floorplan = nil

	priv := genTestKey(t)
	const kid = "nofloorplanrooms"
	id := fakeJWKSServer(t, &priv.PublicKey, kid)
	s.IdentityURL = id.URL
	q.QueryFunc = func(string) ([]influx.Row, error) {
		return bucketRows(t, s, "sensor_e", "today", "1h", 20), nil
	}

	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	token := serviceToken(t, priv, kid, id.URL, time.Now().Add(time.Hour))

	var body roomsBody
	get(t, srv.URL, token, "/rooms", http.StatusOK, &body)
	if len(body.Rooms) != 4 {
		t.Fatalf("want the 4 climate rooms regardless, got %v", body.Rooms)
	}
	for _, r := range body.Rooms {
		if r.Name != "" || r.Category != "" || r.Area != nil {
			t.Errorf("%s carries floorplan detail with none configured: %+v", r.ID, r)
		}
	}

	var resp struct {
		Series []struct {
			Key   string `json:"key"`
			Label string `json:"label"`
		} `json:"series"`
	}
	get(t, srv.URL, token, "/series?window=today&interval=1h&group_by=room",
		http.StatusOK, &resp)
	for _, ser := range resp.Series {
		if ser.Label != ser.Key {
			t.Errorf("%s labelled %q, want the id with no floorplan", ser.Key, ser.Label)
		}
	}
}

// The spec documents the endpoint and, importantly, that `area` is nullable — a
// consumer generating types from a non-nullable number would fail to parse a
// real response for a room the floorplan has not measured.
func TestIntegration_SpecDocumentsRoomsEndpoint(t *testing.T) {
	base, _, _ := roomsIntegrationServer(t)

	var spec struct {
		Paths map[string]struct {
			Get struct {
				OperationID string `json:"operationId"`
			} `json:"get"`
		} `json:"paths"`
		Components struct {
			Schemas map[string]struct {
				Properties map[string]struct {
					Type     string `json:"type"`
					Nullable bool   `json:"nullable"`
				} `json:"properties"`
				Required []string `json:"required"`
			} `json:"schemas"`
		} `json:"components"`
	}
	get(t, base, "", "/openapi.json", http.StatusOK, &spec)

	p, ok := spec.Paths["/rooms"]
	if !ok {
		t.Fatal("spec documents no /rooms path")
	}
	if p.Get.OperationID != "getRooms" {
		t.Errorf("operationId = %q, want getRooms", p.Get.OperationID)
	}

	entry, ok := spec.Components.Schemas["RoomCatalogEntry"]
	if !ok {
		t.Fatal("spec defines no RoomCatalogEntry schema")
	}
	if area, ok := entry.Properties["area"]; !ok {
		t.Error("RoomCatalogEntry does not document 'area'")
	} else if !area.Nullable {
		t.Error("'area' must be documented nullable: it is null whenever the " +
			"floorplan has not measured the room")
	}
	// category must be a plain string, not a boolean masquerading as policy.
	if cat, ok := entry.Properties["category"]; !ok {
		t.Error("RoomCatalogEntry does not document 'category'")
	} else if cat.Type != "string" {
		t.Errorf("category type = %q, want string: the raw classification, not a flag",
			cat.Type)
	}
	for _, field := range []string{"id", "name", "floor", "category", "area", "device_count"} {
		if !slices.Contains(entry.Required, field) {
			t.Errorf("%q is always present in the response, so it must be required: %v",
				field, entry.Required)
		}
	}
}
