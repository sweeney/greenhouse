package config

import (
	"encoding/json"
	"testing"
)

func TestNormaliseDevicesLegacyShorthand(t *testing.T) {
	devices := map[string]DeviceConfig{
		"probe_a": {
			IEEEAddress:  "0x00124b00",
			FriendlyName: "Glow Sensor",
			Class:        "environmental_sensor",
		},
	}
	normaliseDevices(devices)
	d := devices["probe_a"]
	if d.Scheme != "zigbee" {
		t.Errorf("scheme = %q, want zigbee", d.Scheme)
	}
	if d.Primary != "0x00124b00" {
		t.Errorf("primary = %q, want ieee address", d.Primary)
	}
	if d.Display != "Glow Sensor" {
		t.Errorf("display = %q, want friendly name", d.Display)
	}
}

func TestNormaliseDevicesDoesNotOverrideCanonical(t *testing.T) {
	devices := map[string]DeviceConfig{
		"outdoor_station": {
			Scheme:       "tasmota",
			Primary:      "tasmota-123",
			Display:      "Canonical Name",
			IEEEAddress:  "0xdead",
			FriendlyName: "Legacy Name",
		},
	}
	normaliseDevices(devices)
	d := devices["outdoor_station"]
	if d.Scheme != "tasmota" {
		t.Errorf("scheme = %q, want tasmota preserved", d.Scheme)
	}
	if d.Primary != "tasmota-123" {
		t.Errorf("primary = %q, want canonical preserved", d.Primary)
	}
	if d.Display != "Canonical Name" {
		t.Errorf("display = %q, want canonical preserved", d.Display)
	}
}

func TestNormaliseDevicesNoLegacyFields(t *testing.T) {
	devices := map[string]DeviceConfig{
		"sensor_a": {Class: "environmental_sensor"},
	}
	normaliseDevices(devices)
	d := devices["sensor_a"]
	if d.Scheme != "" {
		t.Errorf("scheme = %q, want empty (no legacy fields to imply zigbee)", d.Scheme)
	}
}

// --- ReportsEnvironment (the single climate-device predicate) ---

// The class allowlist is the one place that decides what greenhouse charts.
// Pinning it as a table makes any future widening an explicit, reviewed edit
// rather than an incidental one.
func TestReportsEnvironment(t *testing.T) {
	tests := []struct {
		name  string
		class string
		want  bool
	}{
		{"purpose-built sensor", "environmental_sensor", true},
		// Fire alarms report temperature_c alongside their smoke state, and in
		// those rooms they are the ONLY environment source.
		{"fire alarm", "fire_alarm", true},
		{"power device", "continuous_power_device", false},
		{"cycle power device", "cycle_power_device", false},
		{"energy meter", "energy_meter", false},
		{"binary state device", "binary_state_device", false},
		{"ups sensor", "ups_sensor", false},
		{"media power device", "media_power_device", false},
		{"unknown class", "something_new", false},
		{"empty class", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := DeviceConfig{Class: tt.class}
			if got := d.ReportsEnvironment(); got != tt.want {
				t.Errorf("ReportsEnvironment() for class %q = %v, want %v", tt.class, got, tt.want)
			}
		})
	}
}

// The predicate keys off class alone: a declared environment_fields list does
// NOT make a non-climate device chartable under the current (option A) rule.
// This is the documented limitation — if the selection rule ever moves to
// environment_fields, this test is the one that should change.
func TestReportsEnvironment_IgnoresEnvironmentFields(t *testing.T) {
	d := DeviceConfig{Class: "continuous_power_device", EnvironmentFields: []string{"temperature_c"}}
	if d.ReportsEnvironment() {
		t.Error("a non-climate class with environment_fields must not be charted under the class allowlist")
	}
	// ...and conversely, a climate class with no hint is still charted.
	d = DeviceConfig{Class: "fire_alarm"}
	if !d.ReportsEnvironment() {
		t.Error("a climate class must be charted even without an environment_fields hint")
	}
}

// environment_fields round-trips from the shared namespace's JSON shape.
func TestEnvironmentFieldsJSONKey(t *testing.T) {
	var d DeviceConfig
	body := `{"class":"fire_alarm","environment_fields":["temperature_c"]}`
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(d.EnvironmentFields) != 1 || d.EnvironmentFields[0] != "temperature_c" {
		t.Fatalf("environment_fields = %v, want [temperature_c]", d.EnvironmentFields)
	}
	// The old generic key must NOT populate it — a stale namespace should fail
	// loudly via the registry fallback, not silently half-work.
	d = DeviceConfig{}
	if err := json.Unmarshal([]byte(`{"fields":["temperature_c"]}`), &d); err != nil {
		t.Fatalf("unmarshal legacy: %v", err)
	}
	if d.EnvironmentFields != nil {
		t.Errorf("legacy `fields` key must not populate EnvironmentFields, got %v", d.EnvironmentFields)
	}
}

// --- MayReportField (per-device field coverage) ---

func TestMayReportField(t *testing.T) {
	declared := DeviceConfig{
		Class:             "environmental_sensor",
		EnvironmentFields: []string{"temperature_c", "humidity_pct"},
	}
	if !declared.MayReportField("temperature_c") {
		t.Error("a declared field must be reportable")
	}
	if declared.MayReportField("rainfall_mm") {
		t.Error("a field outside an explicit declaration must not be reportable")
	}

	// The permissive case: no declaration means coverage is UNKNOWN, so every
	// field is allowed. This is what stops a stale namespace from hiding real
	// readings — see the doc comment on MayReportField.
	undeclared := DeviceConfig{Class: "environmental_sensor"}
	for _, f := range []string{"temperature_c", "rainfall_mm", "uv_index"} {
		if !undeclared.MayReportField(f) {
			t.Errorf("undeclared coverage must permit %q", f)
		}
	}

	// An explicitly empty (non-nil) list is still "no declaration".
	empty := DeviceConfig{Class: "environmental_sensor", EnvironmentFields: []string{}}
	if !empty.MayReportField("rainfall_mm") {
		t.Error("an empty list must behave as undeclared, not as 'reports nothing'")
	}
}

// Floor is a first-class property of the devices namespace, so it is DECODED,
// never derived: the floorplan owns the fact, and re-deriving it from the room id
// would be a second implementation of someone else's taxonomy.
func TestDeviceConfig_FloorDecodesFromNamespace(t *testing.T) {
	// The remote namespace is JSON, and this is the shape it publishes: floor
	// alongside room, not encoded inside it.
	var d DeviceConfig
	body := `{"class":"environmental_sensor","floor":"floor2","room":"floor2.room-a"}`
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d.Floor != "floor2" {
		t.Errorf("Floor = %q, want floor2", d.Floor)
	}
	if d.Room != "floor2.room-a" {
		t.Errorf("Room = %q, want floor2.room-a", d.Room)
	}
}

// An undeclared floor is UNKNOWN, and greenhouse leaves it empty rather than
// reaching into the room id for a guess. Nothing downstream may invent one: the
// catalog reports it empty and the floors= filter never matches it.
func TestDeviceConfig_FloorIsEmptyWhenUndeclared(t *testing.T) {
	var d DeviceConfig
	if err := json.Unmarshal([]byte(`{"class":"environmental_sensor","room":"floor2.room-a"}`), &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d.Floor != "" {
		t.Errorf("Floor = %q, want empty: the namespace declared none", d.Floor)
	}
}

// A floor may be declared for a device that has no room id at all — the two are
// independent properties, so neither implies the other.
func TestDeviceConfig_FloorWithoutRoom(t *testing.T) {
	var d DeviceConfig
	if err := json.Unmarshal([]byte(`{"class":"environmental_sensor","floor":"floor1"}`), &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d.Floor != "floor1" {
		t.Errorf("Floor = %q, want floor1", d.Floor)
	}
	if d.Place() != "" {
		t.Errorf("Place() = %q, want empty: no room and no location", d.Place())
	}
}
