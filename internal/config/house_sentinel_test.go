package config

import "testing"

// The legacy free-text `location` carried two different facts. Usually a place, but
// for the whole-property devices it carried `house`, which is a coverage scope and
// not a room. Resolving it as a room republishes the conflation this migration
// removes, and `house` is a reserved series key the taxonomy forbids as a room id.
//
// Greenhouse charts only climate classes, none of which are house-scoped today, so
// this is a latent case rather than a live one — but Place() is the shared shim and
// must not answer with a non-room.
func TestLegacyHouseLocationIsNotARoom(t *testing.T) {
	if got := (DeviceConfig{Location: "house"}).Place(); got != "" {
		t.Errorf("Place() = %q, want empty: `house` is a scope, not a room", got)
	}
}

func TestOrdinaryPlacesAreUnaffected(t *testing.T) {
	for _, d := range []DeviceConfig{{Location: "area-e"}, {Room: "floor2.room-a"}} {
		if d.Place() == "" {
			t.Errorf("%+v: Place() lost the room", d)
		}
	}
}
