package config

// RoomConfig is one room record as the floorplan namespace publishes it.
//
// Greenhouse passes these through unchanged, exactly as it does floor records
// and the `floor` a device declares. The floorplan owns the taxonomy; greenhouse
// relays it; clients interpret it. A client that had to title-case a room id
// into a label, or guess a room's purpose from its slug, would be a second
// implementation of someone else's data — living in every consumer instead of in
// one service, and wrong the moment a room is renamed upstream.
//
// Every field is optional. A namespace publishing only names still improves on
// nothing, and an omitted field is UNKNOWN rather than a value greenhouse
// invented.
type RoomConfig struct {
	// ID is the floorplan room id devices reference in their `room` property and
	// callers pass to `rooms=`, e.g. "floor2.room-a". It is the map key in the
	// namespace document; normaliseRooms fills it from the key when a record
	// omits it, and the KEY always wins so the two can never disagree.
	ID string `yaml:"id" json:"id,omitempty"`

	// Name is the human-readable label, e.g. "Room A". Empty when the namespace
	// declares none: the catalog reports it empty rather than deriving one from
	// the id.
	//
	// Names are NOT unique — two rooms on different floors may share one. The id
	// is the key, and a client wanting an unambiguous label composes the floor's
	// name with this one.
	Name string `yaml:"name" json:"name,omitempty"`

	// Floor is the floor id this room sits on. Passed through from the room
	// record, never read out of the id's "<floor>.<slug>" shape and never
	// inferred from the floors its devices declare: those are separate
	// declarations that can disagree, and greenhouse does not arbitrate between
	// two upstream answers. Empty means the floorplan declared none.
	Floor string `yaml:"floor" json:"floor,omitempty"`

	// Category is the room's purpose as the floorplan classifies it, e.g.
	// "kitchen", "circulation", "plant". Relayed RAW and never reduced to a
	// boolean: whether a plant room "counts" is a per-client policy question, not
	// a fact about the room. A floor-mean view excludes it, a "where is the heat
	// going?" view wants it, and an equipment view wants only it — a computed
	// flag would bake the first caller's answer into the API and leave the other
	// two working around it.
	Category string `yaml:"category" json:"category,omitempty"`

	// Area is the room's floor area in square metres. A POINTER because absence
	// differs from zero, the same reason FloorConfig.Order and Elevation are:
	// nil is UNKNOWN and the catalog reports it null.
	Area *float64 `yaml:"area" json:"area,omitempty"`
}

// normaliseRooms fills each record's ID from its map key.
//
// The key is AUTHORITATIVE: it is what a device's `room` property references and
// what `rooms=` matches, so a record whose inner id disagrees with its key would
// otherwise publish an id nothing can be filtered by. Mirrors normaliseFloors
// and normaliseDevices, which treat their keys the same way.
func normaliseRooms(rooms map[string]RoomConfig) {
	for id, r := range rooms {
		r.ID = id
		rooms[id] = r
	}
}
