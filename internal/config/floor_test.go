package config

import (
	"encoding/json"
	"testing"
)

// Floor records are PASSED THROUGH from the floorplan namespace, never derived.
// A client that had to sort floor ids into building order, or title-case one
// into a label, would be a second implementation of someone else's taxonomy —
// living in every consumer instead of in one service.

func TestFloorConfig_DecodesFromNamespace(t *testing.T) {
	// The shape the config service publishes, per the issue.
	body := `{"id":"floor1","name":"Lower Floor","order":1,"elevation":0.0}`
	var f FloorConfig
	if err := json.Unmarshal([]byte(body), &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if f.ID != "floor1" {
		t.Errorf("ID = %q, want floor1", f.ID)
	}
	if f.Name != "Lower Floor" {
		t.Errorf("Name = %q, want the declared label", f.Name)
	}
	if f.Order == nil || *f.Order != 1 {
		t.Errorf("Order = %v, want 1", f.Order)
	}
	if f.Elevation == nil || *f.Elevation != 0.0 {
		t.Errorf("Elevation = %v, want 0.0", f.Elevation)
	}
}

// Order is a POINTER because absence differs from zero: a basement legitimately
// sits at order 0, so a plain int could not tell "ground level" from
// "undeclared". This is the whole reason the field is nullable.
func TestFloorConfig_ZeroOrderIsNotUndeclared(t *testing.T) {
	var declared, undeclared FloorConfig
	if err := json.Unmarshal([]byte(`{"order":0}`), &declared); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{}`), &undeclared); err != nil {
		t.Fatal(err)
	}
	if declared.Order == nil {
		t.Error("order 0 must decode as a DECLARED zero, not as absent — a basement sits at 0")
	} else if *declared.Order != 0 {
		t.Errorf("Order = %d, want 0", *declared.Order)
	}
	if undeclared.Order != nil {
		t.Errorf("an omitted order must stay nil, got %v", *undeclared.Order)
	}
}

// Same argument for elevation: 0.0 is a real height above the datum.
func TestFloorConfig_ZeroElevationIsNotUndeclared(t *testing.T) {
	var f FloorConfig
	if err := json.Unmarshal([]byte(`{"elevation":0.0}`), &f); err != nil {
		t.Fatal(err)
	}
	if f.Elevation == nil {
		t.Error("elevation 0.0 must decode as declared, not absent")
	}
}

// A namespace declaring only a name is still an improvement on nothing, so every
// field is independently optional.
func TestFloorConfig_PartialRecordDecodes(t *testing.T) {
	var f FloorConfig
	if err := json.Unmarshal([]byte(`{"name":"Attic"}`), &f); err != nil {
		t.Fatal(err)
	}
	if f.Name != "Attic" {
		t.Errorf("Name = %q, want Attic", f.Name)
	}
	if f.Order != nil || f.Elevation != nil {
		t.Error("undeclared order/elevation must stay nil rather than defaulting")
	}
}

// The map key is what devices reference and what floors= matches, so it is
// AUTHORITATIVE. A record with no inner id gets it from the key.
func TestNormaliseFloors_FillsIDFromKey(t *testing.T) {
	floors := map[string]FloorConfig{
		"floor1": {Name: "Lower Floor"},
	}
	normaliseFloors(floors)
	if got := floors["floor1"].ID; got != "floor1" {
		t.Errorf("ID = %q, want it filled from the key", got)
	}
}

// And a record whose inner id CONTRADICTS its key is corrected to the key —
// otherwise the catalog would publish an id that nothing can be filtered by.
func TestNormaliseFloors_KeyWinsOverAContradictoryInnerID(t *testing.T) {
	floors := map[string]FloorConfig{
		"floor1": {ID: "somethingelse", Name: "Lower Floor"},
	}
	normaliseFloors(floors)
	if got := floors["floor1"].ID; got != "floor1" {
		t.Errorf("ID = %q, want the authoritative key floor1: devices reference the "+
			"key, so an inner id that disagrees is unusable", got)
	}
}

// The whole namespace document decodes as a keyed map, mirroring devices.
func TestFloorConfig_NamespaceDocumentDecodes(t *testing.T) {
	body := `{
		"floor1": {"name": "Lower Floor", "order": 1, "elevation": 0.0},
		"floor2": {"name": "Upper Floor", "order": 2, "elevation": 3.2},
		"floor3": {"name": "Top Floor"}
	}`
	var floors map[string]FloorConfig
	if err := json.Unmarshal([]byte(body), &floors); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	normaliseFloors(floors)

	if len(floors) != 3 {
		t.Fatalf("got %d floors, want 3", len(floors))
	}
	if floors["floor2"].Order == nil || *floors["floor2"].Order != 2 {
		t.Errorf("floor2 order = %v, want 2", floors["floor2"].Order)
	}
	// floor3 declares no order: unknown, not zero.
	if floors["floor3"].Order != nil {
		t.Errorf("floor3 order = %v, want nil", *floors["floor3"].Order)
	}
	if floors["floor3"].ID != "floor3" {
		t.Errorf("floor3 ID = %q, want it normalised from the key", floors["floor3"].ID)
	}
}
