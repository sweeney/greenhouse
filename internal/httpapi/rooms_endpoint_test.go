package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sweeney/greenhouse/internal/config"
	"github.com/sweeney/greenhouse/internal/influx"
)

// /rooms is the discoverable vocabulary behind rooms= — the room-shaped sibling
// of /floors — and the source of the display names group_by=room labels with.
//
// Three client-side derivations motivated it: a name transform (split the id,
// title-case, hope), a purpose heuristic (match substrings against the slug to
// spot a plant room), and a hardcoded exclusion list. Each is a reimplementation
// of a taxonomy the floorplan already publishes.

// roomCatalogDevices spans two floors and includes the cases that make the endpoint's
// rules observable: a room with two sensors, a plant room, a non-climate device
// alone in a room, and a device with no room at all.
func roomCatalogDevices() map[string]config.DeviceConfig {
	return map[string]config.DeviceConfig{
		"sensor_e": {Class: "environmental_sensor", Room: "floor1.room-a", Floor: "floor1"},
		"sensor_f": {Class: "environmental_sensor", Room: "floor1.room-a", Floor: "floor1"},
		"probe_a":  {Class: "environmental_sensor", Room: "floor1.room-c", Floor: "floor1"},
		"sensor_g": {Class: "environmental_sensor", Room: "floor2.room-a", Floor: "floor2"},
		"alarm_a":  {Class: "fire_alarm", Room: "floor2.room-b", Floor: "floor2"},
		// Non-climate, alone in its room: floor2.room-z must not be listed.
		"plug_a": {Class: "continuous_power_device", Room: "floor2.room-z", Floor: "floor2"},
		// No room at all: UNKNOWN, contributes to no room.
		"sensor_x": {Class: "environmental_sensor", Floor: "floor2"},
	}
}

// roomCatalogRecords describes three of the four climate rooms; floor2.room-b has no
// record (so it is listed with unknowns), and floor9.room-a has a record but no
// device (so it is omitted).
func roomCatalogRecords() map[string]config.RoomConfig {
	return map[string]config.RoomConfig{
		"floor1.room-a": {
			ID: "floor1.room-a", Name: "Room A", Floor: "floor1",
			Category: "utility", Area: f64Ptr(12.4),
		},
		"floor1.room-c": {
			ID: "floor1.room-c", Name: "Room C", Floor: "floor1",
			Category: "plant", Area: f64Ptr(1.3),
		},
		"floor2.room-a": {
			ID: "floor2.room-a", Name: "Room A", Floor: "floor2",
			Category: "kitchen", Area: f64Ptr(25.9),
		},
		"floor9.room-a": {ID: "floor9.room-a", Name: "Ghost Room", Floor: "floor9"},
	}
}

func roomCatalogFloors() map[string]config.FloorConfig {
	return map[string]config.FloorConfig{
		"floor1": {ID: "floor1", Name: "Lower Floor", Order: intPtr(1)},
		"floor2": {ID: "floor2", Name: "Upper Floor", Order: intPtr(2)},
	}
}

type roomRow struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Floor       string   `json:"floor"`
	Category    string   `json:"category"`
	Area        *float64 `json:"area"`
	DeviceCount int      `json:"device_count"`
}

func roomSetup(t *testing.T) (*Server, *influx.FakeQuerier) {
	t.Helper()
	s, q := dataSetup(t)
	s.Config = fakeConfig{devices: roomCatalogDevices()}
	s.Floorplan = fakeFloors{floors: roomCatalogFloors(), rooms: roomCatalogRecords()}
	q.QueryFunc = func(string) ([]influx.Row, error) {
		var rows []influx.Row
		for _, id := range []string{"sensor_e", "sensor_f", "probe_a", "sensor_g", "alarm_a", "sensor_x"} {
			rows = append(rows, bucketRows(t, s, id, "today", "1h", 20)...)
		}
		return rows, nil
	}
	return s, q
}

func getRooms(t *testing.T, s *Server) []roomRow {
	t.Helper()
	w := doGET(t, s, "/rooms")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /rooms: want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Rooms []roomRow `json:"rooms"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %s: %v", w.Body.String(), err)
	}
	return resp.Rooms
}

func roomByID(rows []roomRow, id string) (roomRow, bool) {
	for _, r := range rows {
		if r.ID == id {
			return r, true
		}
	}
	return roomRow{}, false
}

// The declared name, floor, category and area are passed through unchanged.
func TestRooms_PassesThroughTheDeclaredRecord(t *testing.T) {
	s, _ := roomSetup(t)
	got := getRooms(t, s)

	r, ok := roomByID(got, "floor1.room-a")
	if !ok {
		t.Fatalf("no floor1.room-a in %v", got)
	}
	if r.Name != "Room A" {
		t.Errorf("name = %q, want the floorplan's label", r.Name)
	}
	if r.Floor != "floor1" {
		t.Errorf("floor = %q, want floor1", r.Floor)
	}
	if r.Category != "utility" {
		t.Errorf("category = %q, want utility", r.Category)
	}
	if r.Area == nil || *r.Area != 12.4 {
		t.Errorf("area = %v, want 12.4", r.Area)
	}
}

// The case that motivated the request: a plant room is identifiable from the API
// instead of by matching substrings against the room id's slug.
func TestRooms_PlantRoomIsIdentifiableByCategory(t *testing.T) {
	s, _ := roomSetup(t)
	got := getRooms(t, s)

	r, ok := roomByID(got, "floor1.room-c")
	if !ok {
		t.Fatal("no floor1.room-c")
	}
	if r.Category != "plant" {
		t.Errorf("category = %q, want plant — the fact a client currently infers "+
			"from the id's slug", r.Category)
	}
}

// Category is relayed RAW, never reduced to a computed flag: whether a plant
// room "counts" is a per-client policy question, not a fact about the room.
func TestRooms_NoComputedPolicyFlags(t *testing.T) {
	s, _ := roomSetup(t)
	w := doGET(t, s, "/rooms")

	var resp struct {
		Rooms []map[string]any `json:"rooms"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"id": true, "name": true, "floor": true,
		"category": true, "area": true, "device_count": true,
	}
	for _, row := range resp.Rooms {
		for key := range row {
			if !want[key] {
				t.Errorf("unexpected field %q: the catalog relays the floorplan's "+
					"facts and computes no policy", key)
			}
		}
	}
}

// A room devices sit in that the floorplan has no record for is LISTED, with its
// fields empty and null. Omitting it would hide a chartable room; inventing a
// name would be the derivation this endpoint exists to remove.
func TestRooms_UnknownRecordIsListedWithBlanks(t *testing.T) {
	s, _ := roomSetup(t)
	got := getRooms(t, s)

	r, ok := roomByID(got, "floor2.room-b")
	if !ok {
		t.Fatalf("floor2.room-b holds a climate device and must be listed: %v", got)
	}
	if r.Name != "" || r.Floor != "" || r.Category != "" {
		t.Errorf("got %+v, want name/floor/category empty: the floorplan declares none", r)
	}
	if r.Area != nil {
		t.Errorf("area = %v, want null", *r.Area)
	}
	if r.DeviceCount != 1 {
		t.Errorf("device_count = %d, want 1 — the count is ours, not the floorplan's",
			r.DeviceCount)
	}
}

// A floorplan record for a room with no climate device is not listed: rooms=
// would reject it, and a picker filled from this endpoint must never 400.
func TestRooms_OmitsRoomsWithNoClimateDevice(t *testing.T) {
	s, _ := roomSetup(t)
	if _, ok := roomByID(getRooms(t, s), "floor9.room-a"); ok {
		t.Error("floor9.room-a has a record but no climate device: listing it would " +
			"advertise a value rooms= rejects")
	}
}

// Class is applied first: a room holding only a plug is not a climate room.
func TestRooms_OmitsRoomsHoldingOnlyNonClimateDevices(t *testing.T) {
	s, _ := roomSetup(t)
	if _, ok := roomByID(getRooms(t, s), "floor2.room-z"); ok {
		t.Error("floor2.room-z holds only a plug and must not be listed")
	}
}

// A device with no room is UNKNOWN, not a room of its own.
func TestRooms_RoomlessDeviceIsNotARoom(t *testing.T) {
	s, _ := roomSetup(t)
	for _, r := range getRooms(t, s) {
		if r.ID == "" {
			t.Error("a room was listed with an empty id")
		}
	}
}

// The listing is exactly the rooms= vocabulary. Asserted as a set so the two
// cannot drift: this is the endpoint's whole contract.
func TestRooms_ListsExactlyTheRoomsFilterVocabulary(t *testing.T) {
	s, _ := roomSetup(t)
	got := getRooms(t, s)

	want := map[string]bool{
		"floor1.room-a": true, "floor1.room-c": true,
		"floor2.room-a": true, "floor2.room-b": true,
	}
	if len(got) != len(want) {
		t.Fatalf("listed %d rooms, want %d: %v", len(got), len(want), got)
	}
	for _, r := range got {
		if !want[r.ID] {
			t.Errorf("unexpected room %q", r.ID)
		}
	}
}

// Every listed room must be accepted by rooms= on /series — the same
// two-endpoint agreement /floors has with floors=.
func TestRooms_EveryListedRoomIsAcceptedByTheFilter(t *testing.T) {
	s, _ := roomSetup(t)
	for _, r := range getRooms(t, s) {
		t.Run(r.ID, func(t *testing.T) {
			w := doGET(t, s, "/series?window=today&interval=1h&rooms="+r.ID)
			if w.Code != http.StatusOK {
				t.Errorf("rooms=%s: /rooms advertised it but /series returned %d: %s",
					r.ID, w.Code, w.Body.String())
			}
		})
	}
}

func TestRooms_DeviceCount(t *testing.T) {
	s, _ := roomSetup(t)
	got := getRooms(t, s)

	for id, want := range map[string]int{
		"floor1.room-a": 2, "floor1.room-c": 1, "floor2.room-a": 1, "floor2.room-b": 1,
	} {
		r, ok := roomByID(got, id)
		if !ok {
			t.Fatalf("no %s", id)
		}
		if r.DeviceCount != want {
			t.Errorf("%s device_count = %d, want %d", id, r.DeviceCount, want)
		}
	}
}

// Ordering: by floor in the order /floors reports floors, then by room id — so a
// client renders the list building-order top to bottom without re-sorting.
func TestRooms_SortedByFloorOrderThenRoomID(t *testing.T) {
	s, _ := roomSetup(t)
	got := getRooms(t, s)

	// floor1 (order 1) before floor2 (order 2); within floor1, room-a before
	// room-c; floor2.room-b has no record so its floor is unknown and it sorts
	// last.
	want := []string{"floor1.room-a", "floor1.room-c", "floor2.room-a", "floor2.room-b"}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("position %d = %q, want %q (got %v)", i, got[i].ID, id, got)
		}
	}
}

// A room whose floor is UNKNOWN sorts after every known floor, so unplaced rooms
// gather at the end rather than leading the list.
func TestRooms_UnknownFloorSortsLast(t *testing.T) {
	s, _ := dataSetup(t)
	s.Config = fakeConfig{devices: map[string]config.DeviceConfig{
		"sensor_e": {Class: "environmental_sensor", Room: "aaa.unplaced"},
		"sensor_f": {Class: "environmental_sensor", Room: "zzz.placed"},
	}}
	s.Floorplan = fakeFloors{
		floors: map[string]config.FloorConfig{"floor2": {Order: intPtr(2)}},
		rooms:  map[string]config.RoomConfig{"zzz.placed": {Floor: "floor2"}},
	}

	got := getRooms(t, s)
	// Sorted by id alone, "aaa.unplaced" would come first. Its floor is unknown,
	// so it must not.
	if got[0].ID != "zzz.placed" {
		t.Errorf("first = %q, want the room with a known floor", got[0].ID)
	}
}

// Ordering is deterministic across repeated calls, including where every floor
// is unknown and only the id tiebreak applies.
func TestRooms_DeterministicOrder(t *testing.T) {
	s, _ := roomSetup(t)
	s.Floorplan = nil

	want := []string{"floor1.room-a", "floor1.room-c", "floor2.room-a", "floor2.room-b"}
	for i := 0; i < 50; i++ {
		got := getRooms(t, s)
		for j, id := range want {
			if got[j].ID != id {
				t.Fatalf("iteration %d position %d = %q, want %q", i, j, got[j].ID, id)
			}
		}
	}
}

// No floorplan configured at all — the common case for an instance that never
// sets floorplan_namespace. Every climate room is still listed, with unknowns.
func TestRooms_NilProviderStillListsRooms(t *testing.T) {
	s, _ := roomSetup(t)
	s.Floorplan = nil

	got := getRooms(t, s)
	if len(got) != 4 {
		t.Fatalf("want the 4 climate rooms even with no floorplan, got %v", got)
	}
	for _, r := range got {
		if r.Name != "" || r.Category != "" || r.Area != nil {
			t.Errorf("%s carries %q/%q/%v with no floorplan configured",
				r.ID, r.Name, r.Category, r.Area)
		}
		if r.DeviceCount == 0 {
			t.Errorf("%s device_count = 0, want the real count", r.ID)
		}
	}
}

// An inventory with no climate device is an empty list, not an error, and the
// key is present so a client can iterate without a nil check.
func TestRooms_NoClimateDevicesIsAnEmptyList(t *testing.T) {
	s, _ := dataSetup(t)
	s.Config = fakeConfig{devices: map[string]config.DeviceConfig{
		"plug_a": {Class: "continuous_power_device", Room: "floor1.room-a"},
	}}

	w := doGET(t, s, "/rooms")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if string(resp["rooms"]) != "[]" {
		t.Errorf("rooms = %s, want an empty array rather than null", resp["rooms"])
	}
}

// Unknown fields are present as empty/null rather than absent, so a client never
// has to distinguish "absent" from "unknown".
func TestRooms_UnknownFieldsArePresent(t *testing.T) {
	s, _ := roomSetup(t)
	w := doGET(t, s, "/rooms")

	var resp struct {
		Rooms []map[string]json.RawMessage `json:"rooms"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for _, row := range resp.Rooms {
		for _, key := range []string{"id", "name", "floor", "category", "area", "device_count"} {
			if _, ok := row[key]; !ok {
				t.Errorf("row %s missing key %q", row["id"], key)
			}
		}
		if string(row["id"]) == `"floor2.room-b"` {
			if string(row["area"]) != "null" {
				t.Errorf("floor2.room-b area = %s, want null", row["area"])
			}
			if string(row["category"]) != `""` {
				t.Errorf("floor2.room-b category = %s, want an empty string", row["category"])
			}
		}
	}
}

// The catalog uses the same room the rest of the API does (DeviceConfig.Place),
// so a device carrying only the deprecated `location` is still catalogued under
// the place it is grouped and filtered by.
func TestRooms_UsesTheSamePlaceAsGroupingAndFiltering(t *testing.T) {
	s, _ := dataSetup(t)
	s.Config = fakeConfig{devices: map[string]config.DeviceConfig{
		"sensor_a": {Class: "environmental_sensor", Location: "area-a"},
	}}

	got := getRooms(t, s)
	if len(got) != 1 || got[0].ID != "area-a" {
		t.Fatalf("want the legacy location catalogued as its place, got %v", got)
	}
	// And it really is chartable under that key.
	w := doGET(t, s, "/series?window=today&interval=1h&rooms=area-a")
	if w.Code != http.StatusOK {
		t.Errorf("rooms=area-a: want 200, got %d: %s", w.Code, w.Body.String())
	}
}

// --- group_by=room labels ---

// seriesLabels decodes key → label from a columnar series response.
func seriesLabels(t *testing.T, s *Server, query string) map[string]string {
	t.Helper()
	w := doGET(t, s, "/series?window=today&interval=1h&"+query)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /series?%s: want 200, got %d: %s", query, w.Code, w.Body.String())
	}
	var resp struct {
		Series []struct {
			Key   string `json:"key"`
			Label string `json:"label"`
		} `json:"series"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out := map[string]string{}
	for _, ser := range resp.Series {
		out[ser.Key] = ser.Label
	}
	return out
}

// The headline of the request: a legend can render group_by=room without a
// client-side name transform.
func TestSeries_GroupByRoomLabelsWithTheFloorplanName(t *testing.T) {
	s, _ := roomSetup(t)
	got := seriesLabels(t, s, "group_by=room")

	if got["floor1.room-a"] != "Room A" {
		t.Errorf("label = %q, want the floorplan's name", got["floor1.room-a"])
	}
	if got["floor1.room-c"] != "Room C" {
		t.Errorf("label = %q, want Room C", got["floor1.room-c"])
	}
}

// The KEY stays the id. Only the label varies, so a caller matching on identity
// is unaffected by whether a name happens to be published.
func TestSeries_GroupByRoomKeyStaysTheID(t *testing.T) {
	s, _ := roomSetup(t)
	got := seriesLabels(t, s, "group_by=room")

	for _, id := range []string{"floor1.room-a", "floor1.room-c", "floor2.room-a"} {
		if _, ok := got[id]; !ok {
			t.Errorf("no series keyed on %q — the key must remain the room id", id)
		}
	}
}

// A room the floorplan does not name falls back to its id rather than to a label
// derived from it. Greenhouse relays names; it does not invent them.
func TestSeries_GroupByRoomFallsBackToTheIDWhenUnnamed(t *testing.T) {
	s, _ := roomSetup(t)
	got := seriesLabels(t, s, "group_by=room")

	if got["floor2.room-b"] != "floor2.room-b" {
		t.Errorf("label = %q, want the id: the floorplan names this room nothing, "+
			"and a derived label would be the guesswork this replaces",
			got["floor2.room-b"])
	}
}

// An empty declared name is the same as no name: fall back rather than serving a
// blank legend entry.
func TestSeries_GroupByRoomEmptyNameFallsBackToTheID(t *testing.T) {
	s, _ := roomSetup(t)
	s.Floorplan = fakeFloors{rooms: map[string]config.RoomConfig{
		"floor1.room-a": {ID: "floor1.room-a", Name: ""},
	}}
	got := seriesLabels(t, s, "group_by=room")

	if got["floor1.room-a"] != "floor1.room-a" {
		t.Errorf("label = %q, want the id rather than an empty legend entry",
			got["floor1.room-a"])
	}
}

// With no floorplan at all, labels are ids — exactly the behaviour before this
// change, so an instance without the namespace is unaffected.
func TestSeries_GroupByRoomWithoutFloorplanLabelsWithIDs(t *testing.T) {
	s, _ := roomSetup(t)
	s.Floorplan = nil
	got := seriesLabels(t, s, "group_by=room")

	for key, label := range got {
		if key != label {
			t.Errorf("%s labelled %q, want the id with no floorplan configured", key, label)
		}
	}
}

// Two rooms sharing a name both keep it, and stay distinct series: the id is the
// key, so a name collision is not a collision.
func TestSeries_GroupByRoomSharedNamesStayDistinctSeries(t *testing.T) {
	s, _ := roomSetup(t)
	got := seriesLabels(t, s, "group_by=room")

	if got["floor1.room-a"] != "Room A" || got["floor2.room-a"] != "Room A" {
		t.Fatalf("fixture should have two rooms named Room A, got %v", got)
	}
	if len(got) != 4 {
		t.Errorf("got %d series, want 4: same-named rooms must not collapse", len(got))
	}
}

// group_by=floor labels from the floor records for the same reason — the data is
// already published and labelling one axis by name and the other by id would be
// an arbitrary inconsistency.
func TestSeries_GroupByFloorLabelsWithTheFloorplanName(t *testing.T) {
	s, _ := roomSetup(t)
	got := seriesLabels(t, s, "group_by=floor")

	if got["floor1"] != "Lower Floor" {
		t.Errorf("label = %q, want the floor's declared name", got["floor1"])
	}
	if got["floor2"] != "Upper Floor" {
		t.Errorf("label = %q, want Upper Floor", got["floor2"])
	}
}

// group_by=device is untouched: its label has always been the device's
// display_name, and room names have no bearing on it.
func TestSeries_GroupByDeviceLabelIsUnchanged(t *testing.T) {
	s, _ := dataSetup(t)
	s.Config = fakeConfig{devices: map[string]config.DeviceConfig{
		"sensor_e": {
			Class: "environmental_sensor", Room: "floor1.room-a",
			DisplayName: "Sensor E",
		},
	}}
	s.Floorplan = fakeFloors{rooms: map[string]config.RoomConfig{
		"floor1.room-a": {Name: "Room A"},
	}}

	got := seriesLabels(t, s, "group_by=device")
	if got["sensor_e"] != "Sensor E" {
		t.Errorf("label = %q, want the device's display_name — not its room's name",
			got["sensor_e"])
	}
}

// A room record that exists but holds no climate device contributes no label,
// because it produces no series at all.
func TestSeries_UnchartedRoomContributesNoSeries(t *testing.T) {
	s, _ := roomSetup(t)
	got := seriesLabels(t, s, "group_by=room")

	if _, ok := got["floor9.room-a"]; ok {
		t.Error("floor9.room-a has a record but no device, so it must not be a series")
	}
}

// shape=rows shares the assembly step, so it carries the labels too rather than
// this being a quirk of the columnar encoder.
func TestSeries_GroupByRoomLabelsInRowsShape(t *testing.T) {
	s, _ := roomSetup(t)
	w := doGET(t, s, "/series?window=today&interval=1h&group_by=room&shape=rows")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Series []struct {
			Key   string `json:"key"`
			Label string `json:"label"`
		} `json:"series"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var found bool
	for _, ser := range resp.Series {
		if ser.Key == "floor1.room-a" {
			found = true
			if ser.Label != "Room A" {
				t.Errorf("rows label = %q, want Room A", ser.Label)
			}
		}
	}
	if !found {
		t.Error("no floor1.room-a in the rows series metadata")
	}
}
