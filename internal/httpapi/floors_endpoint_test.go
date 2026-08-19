package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sweeney/greenhouse/internal/config"
)

// /floors is the discoverable vocabulary behind floors= and group_by=floor —
// the relationship /fields has to field=.
//
// The contract that matters is WHICH floors it lists: exactly those a climate
// device declares, which is exactly the set floors= accepts. If /floors
// advertised a floor /series rejected, a client filling a picker from this
// endpoint would build a broken control out of correct data.

// fakeFloors is a static FloorProvider for handler tests.
type fakeFloors struct {
	floors map[string]config.FloorConfig
}

func (f fakeFloors) Floors() map[string]config.FloorConfig { return f.floors }

func intPtr(i int) *int         { return &i }
func f64Ptr(f float64) *float64 { return &f }

// catalogFloorDevices spans three floors and includes the two cases that make
// the endpoint's rules observable: a non-climate device on its own floor, and a
// climate device declaring no floor at all.
func catalogFloorDevices() map[string]config.DeviceConfig {
	return map[string]config.DeviceConfig{
		"sensor_e": {Class: "environmental_sensor", Floor: "floor1", Room: "floor1.room-a"},
		"sensor_f": {Class: "environmental_sensor", Floor: "floor1", Room: "floor1.room-b"},
		"sensor_g": {Class: "environmental_sensor", Floor: "floor2", Room: "floor2.room-a"},
		"alarm_a":  {Class: "fire_alarm", Floor: "floor3", Room: "floor3.room-a"},
		// Non-climate, and the ONLY device on floor4: floor4 must not be listed.
		"plug_a": {Class: "continuous_power_device", Floor: "floor4", Room: "floor4.room-a"},
		// Declares no floor: UNKNOWN, contributes to no floor.
		"sensor_x": {Class: "environmental_sensor", Room: "floor5.room-a"},
	}
}

// catalogFloorRecords describes floor1 and floor2 fully, floor3 not at all
// (so it is listed with unknowns), and floor9 which no device is on (so it is
// omitted).
func catalogFloorRecords() map[string]config.FloorConfig {
	return map[string]config.FloorConfig{
		"floor1": {ID: "floor1", Name: "Lower Floor", Order: intPtr(1), Elevation: f64Ptr(0)},
		"floor2": {ID: "floor2", Name: "Upper Floor", Order: intPtr(2), Elevation: f64Ptr(3.2)},
		"floor9": {ID: "floor9", Name: "Sub-basement", Order: intPtr(-1)},
	}
}

type floorRow struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Order       *int     `json:"order"`
	Elevation   *float64 `json:"elevation"`
	DeviceCount int      `json:"device_count"`
}

func floorSetupCatalog(t *testing.T) *Server {
	t.Helper()
	s, _ := dataSetup(t)
	s.Config = fakeConfig{devices: catalogFloorDevices()}
	s.FloorRecords = fakeFloors{floors: catalogFloorRecords()}
	return s
}

func getFloors(t *testing.T, s *Server) []floorRow {
	t.Helper()
	w := doGET(t, s, "/floors")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /floors: want 200, got %d: %s", w.Code, w.Body.String())
	}
	return decodeFloors(t, w)
}

func decodeFloors(t *testing.T, w *httptest.ResponseRecorder) []floorRow {
	t.Helper()
	var resp struct {
		Floors []floorRow `json:"floors"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %s: %v", w.Body.String(), err)
	}
	return resp.Floors
}

func floorByID(rows []floorRow, id string) (floorRow, bool) {
	for _, r := range rows {
		if r.ID == id {
			return r, true
		}
	}
	return floorRow{}, false
}

// The declared name, order and elevation are passed through unchanged. This is
// the point of the endpoint: a client stops title-casing ids and hardcoding
// storey order.
func TestFloors_PassesThroughTheDeclaredRecord(t *testing.T) {
	got := getFloors(t, floorSetupCatalog(t))

	f1, ok := floorByID(got, "floor1")
	if !ok {
		t.Fatalf("no floor1 in %v", got)
	}
	if f1.Name != "Lower Floor" {
		t.Errorf("name = %q, want the floorplan's label", f1.Name)
	}
	if f1.Order == nil || *f1.Order != 1 {
		t.Errorf("order = %v, want 1", f1.Order)
	}
	if f1.Elevation == nil || *f1.Elevation != 0 {
		t.Errorf("elevation = %v, want 0", f1.Elevation)
	}
}

// A floor devices declare but the floorplan has no record for is LISTED, with
// its name empty and order null. Omitting it would hide a chartable floor;
// guessing a name would be the derivation this endpoint exists to remove.
func TestFloors_UnknownRecordIsListedWithNulls(t *testing.T) {
	got := getFloors(t, floorSetupCatalog(t))

	f3, ok := floorByID(got, "floor3")
	if !ok {
		t.Fatalf("floor3 has climate devices and must be listed: %v", got)
	}
	if f3.Name != "" {
		t.Errorf("name = %q, want empty: the floorplan declares none and greenhouse "+
			"must not title-case the id", f3.Name)
	}
	if f3.Order != nil {
		t.Errorf("order = %v, want null: unknown, not a position greenhouse invented", *f3.Order)
	}
	if f3.Elevation != nil {
		t.Errorf("elevation = %v, want null", *f3.Elevation)
	}
}

// A floorplan record for a floor with no CLIMATE device is not listed: floors=
// would reject it, and a picker filled from this endpoint must never be able to
// produce a 400.
func TestFloors_OmitsFloorsWithNoClimateDevice(t *testing.T) {
	got := getFloors(t, floorSetupCatalog(t))

	if _, ok := floorByID(got, "floor9"); ok {
		t.Error("floor9 has a floorplan record but no climate device: listing it would " +
			"advertise a value floors= rejects")
	}
}

// Class is applied first: a floor holding only a plug is not a climate floor.
func TestFloors_OmitsFloorsHoldingOnlyNonClimateDevices(t *testing.T) {
	got := getFloors(t, floorSetupCatalog(t))

	if _, ok := floorByID(got, "floor4"); ok {
		t.Error("floor4 holds only a plug and must not be listed")
	}
}

// A device declaring no floor is UNKNOWN, not a floor of its own — the same rule
// the catalog and floors= already follow.
func TestFloors_UndeclaredFloorIsNotAFloor(t *testing.T) {
	got := getFloors(t, floorSetupCatalog(t))

	for _, r := range got {
		if r.ID == "" {
			t.Error(`a floor was listed with an empty id`)
		}
		if r.ID == "floor5" {
			t.Error("floor5 came from a room id prefix; nothing may derive a floor from a room")
		}
	}
}

// The listing is exactly the floors= vocabulary. Asserted as a set so the two
// cannot drift: this is the endpoint's whole contract.
func TestFloors_ListsExactlyTheFloorsFilterVocabulary(t *testing.T) {
	s := floorSetupCatalog(t)
	got := getFloors(t, s)

	want := map[string]bool{"floor1": true, "floor2": true, "floor3": true}
	if len(got) != len(want) {
		t.Fatalf("listed %d floors, want %d: %v", len(got), len(want), got)
	}
	for _, r := range got {
		if !want[r.ID] {
			t.Errorf("unexpected floor %q", r.ID)
		}
	}
}

// Every listed floor must be accepted by floors= on /series. This is the same
// two-endpoint agreement /devices and /series have, asserted directly.
func TestFloors_EveryListedFloorIsAcceptedByTheFilter(t *testing.T) {
	s := floorSetupCatalog(t)
	for _, r := range getFloors(t, s) {
		t.Run(r.ID, func(t *testing.T) {
			w := doGET(t, s, "/series?window=today&interval=1h&floors="+r.ID)
			if w.Code != http.StatusOK {
				t.Errorf("floors=%s: /floors advertised it but /series returned %d: %s",
					r.ID, w.Code, w.Body.String())
			}
		})
	}
}

// device_count says how many climate devices declare each floor, and counts only
// climate ones.
func TestFloors_DeviceCount(t *testing.T) {
	got := getFloors(t, floorSetupCatalog(t))

	for id, want := range map[string]int{"floor1": 2, "floor2": 1, "floor3": 1} {
		r, ok := floorByID(got, id)
		if !ok {
			t.Fatalf("no %s", id)
		}
		if r.DeviceCount != want {
			t.Errorf("%s device_count = %d, want %d", id, r.DeviceCount, want)
		}
	}
}

// Ordering: declared order ascending, then id. Undeclared orders sort LAST, so a
// client can render the list top to bottom without re-sorting.
func TestFloors_SortedByDeclaredOrderThenID(t *testing.T) {
	got := getFloors(t, floorSetupCatalog(t))

	want := []string{"floor1", "floor2", "floor3"} // floor3 has no order → last
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("position %d = %q, want %q (got %v)", i, got[i].ID, id, got)
		}
	}
}

// A negative order is legitimate (a basement below ground) and must sort first.
func TestFloors_NegativeOrderSortsFirst(t *testing.T) {
	s, _ := dataSetup(t)
	s.Config = fakeConfig{devices: map[string]config.DeviceConfig{
		"sensor_e": {Class: "environmental_sensor", Floor: "floor1"},
		"sensor_b": {Class: "environmental_sensor", Floor: "basement"},
	}}
	s.FloorRecords = fakeFloors{floors: map[string]config.FloorConfig{
		"floor1":   {Order: intPtr(1)},
		"basement": {Order: intPtr(-1)},
	}}

	got := getFloors(t, s)
	if got[0].ID != "basement" {
		t.Errorf("first = %q, want basement at order -1", got[0].ID)
	}
}

// Order 0 is a DECLARED position, not "undeclared", so it sorts before an
// unknown rather than with it. This is why the field is a pointer.
func TestFloors_ZeroOrderSortsAsDeclared(t *testing.T) {
	s, _ := dataSetup(t)
	s.Config = fakeConfig{devices: map[string]config.DeviceConfig{
		"sensor_e": {Class: "environmental_sensor", Floor: "ground"},
		"sensor_f": {Class: "environmental_sensor", Floor: "unknown_order"},
	}}
	s.FloorRecords = fakeFloors{floors: map[string]config.FloorConfig{
		"ground": {Order: intPtr(0)},
		// unknown_order deliberately has no record.
	}}

	got := getFloors(t, s)
	if got[0].ID != "ground" {
		t.Errorf("first = %q, want ground: order 0 is declared, not absent", got[0].ID)
	}
	if got[0].Order == nil || *got[0].Order != 0 {
		t.Errorf("ground order = %v, want a declared 0", got[0].Order)
	}
}

// Two floors with the same declared order fall back to id, so the response is
// deterministic rather than however the map iterated.
func TestFloors_TiedOrdersFallBackToID(t *testing.T) {
	s, _ := dataSetup(t)
	s.Config = fakeConfig{devices: map[string]config.DeviceConfig{
		"sensor_e": {Class: "environmental_sensor", Floor: "floor_b"},
		"sensor_f": {Class: "environmental_sensor", Floor: "floor_a"},
	}}
	s.FloorRecords = fakeFloors{floors: map[string]config.FloorConfig{
		"floor_a": {Order: intPtr(1)},
		"floor_b": {Order: intPtr(1)},
	}}

	for i := 0; i < 50; i++ {
		got := getFloors(t, s)
		if got[0].ID != "floor_a" || got[1].ID != "floor_b" {
			t.Fatalf("iteration %d: order = %v, want a stable floor_a, floor_b", i, got)
		}
	}
}

// The whole listing is deterministic across repeated calls, including the
// all-unknown case where only the id tiebreak applies.
func TestFloors_DeterministicWithNoRecordsAtAll(t *testing.T) {
	s := floorSetupCatalog(t)
	s.FloorRecords = nil

	for i := 0; i < 50; i++ {
		got := getFloors(t, s)
		want := []string{"floor1", "floor2", "floor3"}
		for j, id := range want {
			if got[j].ID != id {
				t.Fatalf("iteration %d position %d = %q, want %q", i, j, got[j].ID, id)
			}
		}
	}
}

// No floorplan provider at all — the common case for an instance that never sets
// floorplan_namespace. Every floor is still listed, with unknowns. A missing
// floorplan must never stop a climate service serving climate.
func TestFloors_NilProviderStillListsFloors(t *testing.T) {
	s := floorSetupCatalog(t)
	s.FloorRecords = nil

	got := getFloors(t, s)
	if len(got) != 3 {
		t.Fatalf("want the 3 climate floors even with no floorplan, got %v", got)
	}
	for _, r := range got {
		if r.Name != "" || r.Order != nil {
			t.Errorf("%s = %q/%v, want unknowns with no floorplan configured", r.ID, r.Name, r.Order)
		}
		if r.DeviceCount == 0 {
			t.Errorf("%s device_count = 0, want the real count", r.ID)
		}
	}
}

// An empty floorplan snapshot behaves identically to no provider.
func TestFloors_EmptyRecordsBehaveAsNilProvider(t *testing.T) {
	s := floorSetupCatalog(t)
	s.FloorRecords = fakeFloors{floors: map[string]config.FloorConfig{}}

	if got := getFloors(t, s); len(got) != 3 {
		t.Fatalf("want 3 floors, got %v", got)
	}
}

// An inventory with no climate device at all is an empty list, not an error, and
// the key is still present so a client can iterate without a nil check.
func TestFloors_NoClimateDevicesIsAnEmptyList(t *testing.T) {
	s, _ := dataSetup(t)
	s.Config = fakeConfig{devices: map[string]config.DeviceConfig{
		"plug_a": {Class: "continuous_power_device", Floor: "floor1"},
	}}

	w := doGET(t, s, "/floors")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	raw, ok := resp["floors"]
	if !ok {
		t.Fatal("response has no 'floors' key")
	}
	if string(raw) != "[]" {
		t.Errorf("floors = %s, want an empty array rather than null", raw)
	}
}

// name is always present (empty when unknown) and order always present (null
// when unknown), so a client never has to distinguish absent from unknown.
func TestFloors_UnknownFieldsArePresentAsEmptyAndNull(t *testing.T) {
	s := floorSetupCatalog(t)
	w := doGET(t, s, "/floors")

	var resp struct {
		Floors []map[string]json.RawMessage `json:"floors"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, row := range resp.Floors {
		for _, key := range []string{"id", "name", "order", "elevation", "device_count"} {
			if _, ok := row[key]; !ok {
				t.Errorf("row %s missing key %q; unknown must be null, not absent", row["id"], key)
			}
		}
	}
	// And floor3's unknowns really are JSON null, not omitted or zero.
	for _, row := range resp.Floors {
		if string(row["id"]) != `"floor3"` {
			continue
		}
		if string(row["order"]) != "null" {
			t.Errorf("floor3 order = %s, want null", row["order"])
		}
		if string(row["name"]) != `""` {
			t.Errorf("floor3 name = %s, want an empty string", row["name"])
		}
	}
}
