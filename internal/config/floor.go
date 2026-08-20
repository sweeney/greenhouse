package config

import (
	"bytes"
	"encoding/json"
)

// FloorConfig is one floor record as the floorplan namespace publishes it.
//
// Greenhouse passes these through unchanged, exactly as it does the `floor` a
// device declares. The floorplan owns the taxonomy: a client that had to sort
// floor ids into building order, or title-case one into a label, would be a
// second implementation of someone else's data — living in every consumer
// instead of in one service, and wrong the moment it met a different building.
//
// Every field is optional. A namespace that publishes only names still improves
// on nothing, and a record that omits Order leaves ordering UNKNOWN rather than
// asserting a position greenhouse invented.
type FloorConfig struct {
	// ID is the floor id devices reference in their `floor` property and callers
	// pass to `floors=`. It is normally the map key in the namespace document;
	// normaliseFloors fills it from the key when a record omits it, and the KEY
	// always wins so the two can never disagree.
	ID string `yaml:"id" json:"id,omitempty"`

	// Name is the human-readable label, e.g. "Ground Floor". Empty when the
	// namespace declares none: the catalog reports it empty rather than
	// title-casing the id and hoping.
	Name string `yaml:"name" json:"name,omitempty"`

	// Order is the position in the building, ascending from the lowest storey.
	// It is a POINTER because absence is meaningful and distinct from zero: a
	// basement legitimately sits at order 0, so a plain int could not tell
	// "ground level" from "undeclared". Nil means UNKNOWN, which the catalog
	// reports as null and sorts last.
	Order *int `yaml:"order" json:"order,omitempty"`

	// Elevation is the floor's height in metres above the site datum, published
	// alongside order. Passed through when declared; nil is UNKNOWN, and it is a
	// pointer for the same reason as Order — 0.0 is a real elevation.
	Elevation *float64 `yaml:"elevation" json:"elevation,omitempty"`
}

// normaliseFloors fills each record's ID from its map key.
//
// The key is AUTHORITATIVE: it is what a device's `floor` property references
// and what `floors=` matches, so a record whose inner id disagrees with its key
// would otherwise publish an id nothing can be filtered by. Mirrors
// normaliseDevices, which treats the device_id key the same way.
func normaliseFloors(floors map[string]FloorConfig) {
	for id, f := range floors {
		f.ID = id
		floors[id] = f
	}
}

// floorplanDocument is the decoded floorplan namespace document: the building's
// floors AND its rooms, each keyed by id, whichever of two shapes the namespace
// publishes.
//
// The published shape wraps ARRAYS, each record carrying its own id:
//
//	{"floors": [{"id": "floor1", "name": "Lower Floor", "order": 1}, ...],
//	 "rooms":  [{"id": "floor1.room-a", "name": "Room A", "floor": "floor1",
//	             "category": "utility", "area": 12.4}, ...]}
//
// The devices namespace, by contrast, is a MAP keyed by id, and greenhouse
// originally assumed the floorplan matched it:
//
//	{"floor1": {"name": "Lower Floor", "order": 1}, ...}
//
// Both are accepted. Being liberal here is not indecision about the format: the
// namespace is owned by another service, greenhouse cannot deploy in lockstep
// with it, and the failure mode of guessing wrong is uniquely bad. A rejected
// document is fail-open by design, so /floors degrades to blank names and null
// order — indistinguishable from a floorplan namespace nobody configured, which
// is exactly how this went unnoticed in prod until /healthz was read.
//
// The shapes are told apart by JSON type, not by key name: a "floors" key
// holding an object is a floor whose id happens to be "floors", and decodes as
// the map shape. The map shape carries FLOORS ONLY — it predates rooms being
// published and there is no room-shaped reading of it, so Rooms is empty there
// and every room's name and category are honestly UNKNOWN rather than guessed.
// Unmodelled keys (e.g. "ceiling") are ignored throughout.
type floorplanDocument struct {
	Floors map[string]FloorConfig
	Rooms  map[string]RoomConfig
}

func (d *floorplanDocument) UnmarshalJSON(b []byte) error {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(b, &probe); err != nil {
		return err
	}

	floorsRaw, hasFloors := probe["floors"]
	roomsRaw, hasRooms := probe["rooms"]

	// The wrapper shape is identified by either collection arriving as an ARRAY.
	// Checking both means a document that publishes rooms but no floors (or vice
	// versa) is still read as the wrapper it is, rather than falling through to
	// the map branch and failing to unmarshal an array into a FloorConfig.
	if (hasFloors && isJSONArray(floorsRaw)) || (hasRooms && isJSONArray(roomsRaw)) {
		out := floorplanDocument{
			Floors: map[string]FloorConfig{},
			Rooms:  map[string]RoomConfig{},
		}
		if hasFloors && isJSONArray(floorsRaw) {
			var list []FloorConfig
			if err := json.Unmarshal(floorsRaw, &list); err != nil {
				return err
			}
			for _, f := range list {
				// A record with no id cannot be referenced by a device's `floor`
				// property or matched by floors=, so it is unusable rather than
				// merely unlabelled. Skipped instead of keyed on "", which would
				// invent a floor nothing can select. A later duplicate wins, as it
				// would in a JSON object.
				if f.ID == "" {
					continue
				}
				out.Floors[f.ID] = f
			}
		}
		if hasRooms && isJSONArray(roomsRaw) {
			var list []RoomConfig
			if err := json.Unmarshal(roomsRaw, &list); err != nil {
				return err
			}
			for _, r := range list {
				// Same rule, same reason: an id-less room is unreferenceable by a
				// device's `room` property and unmatchable by rooms=.
				if r.ID == "" {
					continue
				}
				out.Rooms[r.ID] = r
			}
		}
		*d = out
		return nil
	}

	var keyed map[string]FloorConfig
	if err := json.Unmarshal(b, &keyed); err != nil {
		return err
	}
	*d = floorplanDocument{Floors: keyed}
	return nil
}

// isJSONArray reports whether raw is a JSON array, ignoring leading whitespace.
func isJSONArray(raw json.RawMessage) bool {
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	return len(trimmed) > 0 && trimmed[0] == '['
}
