package config

import (
	"encoding/json"
	"testing"
)

// Room records are PASSED THROUGH from the floorplan namespace, never derived.
// A client that had to title-case a room id into a label, or guess a room's
// purpose from its slug, would be a second implementation of someone else's
// taxonomy — living in every consumer instead of in one service.

func TestRoomConfig_DecodesFromNamespace(t *testing.T) {
	body := `{"id":"floor1.room-a","name":"Room A","floor":"floor1","category":"utility","area":12.4}`
	var r RoomConfig
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if r.ID != "floor1.room-a" {
		t.Errorf("ID = %q, want floor1.room-a", r.ID)
	}
	if r.Name != "Room A" {
		t.Errorf("Name = %q, want the declared label", r.Name)
	}
	if r.Floor != "floor1" {
		t.Errorf("Floor = %q, want floor1", r.Floor)
	}
	if r.Category != "utility" {
		t.Errorf("Category = %q, want utility", r.Category)
	}
	if r.Area == nil || *r.Area != 12.4 {
		t.Errorf("Area = %v, want 12.4", r.Area)
	}
}

// Category is relayed RAW. It is deliberately not reduced to a boolean, because
// whether a plant room "counts" is a per-client policy question — a floor-mean
// view excludes it, a heat-loss view wants it, an equipment view wants only it.
func TestRoomConfig_CategoryIsRelayedRaw(t *testing.T) {
	for _, category := range []string{"kitchen", "circulation", "plant", "utility", "unheard-of"} {
		t.Run(category, func(t *testing.T) {
			var r RoomConfig
			if err := json.Unmarshal([]byte(`{"category":"`+category+`"}`), &r); err != nil {
				t.Fatal(err)
			}
			if r.Category != category {
				t.Errorf("Category = %q, want %q unchanged — greenhouse classifies nothing",
					r.Category, category)
			}
		})
	}
}

// Area is a POINTER because absence differs from zero, the same reason
// FloorConfig.Order and Elevation are.
func TestRoomConfig_AreaAbsenceDiffersFromZero(t *testing.T) {
	var declared, undeclared RoomConfig
	if err := json.Unmarshal([]byte(`{"area":0}`), &declared); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{}`), &undeclared); err != nil {
		t.Fatal(err)
	}
	if declared.Area == nil {
		t.Error("area 0 must decode as a DECLARED zero, not as absent")
	}
	if undeclared.Area != nil {
		t.Errorf("an omitted area must stay nil, got %v", *undeclared.Area)
	}
}

// A namespace declaring only a name is still an improvement on nothing, so every
// field is independently optional and an omitted one stays UNKNOWN.
func TestRoomConfig_PartialRecordDecodes(t *testing.T) {
	var r RoomConfig
	if err := json.Unmarshal([]byte(`{"name":"Room A"}`), &r); err != nil {
		t.Fatal(err)
	}
	if r.Name != "Room A" {
		t.Errorf("Name = %q, want Room A", r.Name)
	}
	if r.Floor != "" || r.Category != "" || r.Area != nil {
		t.Errorf("undeclared fields must stay empty/nil, got floor=%q category=%q area=%v",
			r.Floor, r.Category, r.Area)
	}
}

// The floor is what the room record DECLARES. Nothing reads it out of the id's
// "<floor>.<slug>" shape — the same invariant the device `floor` property has.
func TestRoomConfig_FloorIsDeclaredNotDerived(t *testing.T) {
	var r RoomConfig
	// The id says floor1; the record says floor3. The record wins, because
	// greenhouse does not parse ids for facts.
	if err := json.Unmarshal([]byte(`{"id":"floor1.room-a","floor":"floor3"}`), &r); err != nil {
		t.Fatal(err)
	}
	if r.Floor != "floor3" {
		t.Errorf("Floor = %q, want the DECLARED floor3, not the id's prefix", r.Floor)
	}

	// And a record declaring no floor stays empty rather than gaining one from
	// the id.
	var bare RoomConfig
	if err := json.Unmarshal([]byte(`{"id":"floor1.room-a"}`), &bare); err != nil {
		t.Fatal(err)
	}
	if bare.Floor != "" {
		t.Errorf("Floor = %q, want empty: the record declares none and floor1 would "+
			"be a guess read out of the id", bare.Floor)
	}
}

// Two rooms on different floors may share a name. The id is the key; the name is
// a label, and treating it as an identifier would collapse them.
func TestRoomConfig_NamesAreNotUnique(t *testing.T) {
	body := `[{"id":"floor1.room-a","name":"Room A","floor":"floor1"},
	          {"id":"floor2.room-a","name":"Room A","floor":"floor2"}]`
	var rooms []RoomConfig
	if err := json.Unmarshal([]byte(body), &rooms); err != nil {
		t.Fatal(err)
	}
	if len(rooms) != 2 {
		t.Fatalf("got %d rooms, want 2", len(rooms))
	}
	if rooms[0].Name != rooms[1].Name {
		t.Fatal("fixture should share a name")
	}
	if rooms[0].ID == rooms[1].ID {
		t.Error("ids must differ: the id is the key, the name is not")
	}
}

// The map key is authoritative, mirroring normaliseFloors and normaliseDevices.
func TestNormaliseRooms_FillsIDFromKey(t *testing.T) {
	rooms := map[string]RoomConfig{"floor1.room-a": {Name: "Room A"}}
	normaliseRooms(rooms)
	if got := rooms["floor1.room-a"].ID; got != "floor1.room-a" {
		t.Errorf("ID = %q, want it filled from the key", got)
	}
}

// And a record whose inner id contradicts its key is corrected to the key —
// otherwise the catalog would publish an id nothing can be filtered by.
func TestNormaliseRooms_KeyWinsOverAContradictoryInnerID(t *testing.T) {
	rooms := map[string]RoomConfig{"floor1.room-a": {ID: "somethingelse", Name: "Room A"}}
	normaliseRooms(rooms)
	if got := rooms["floor1.room-a"].ID; got != "floor1.room-a" {
		t.Errorf("ID = %q, want the authoritative key: devices reference the key, so "+
			"an inner id that disagrees is unusable", got)
	}
}
