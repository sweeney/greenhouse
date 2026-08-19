package config

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
