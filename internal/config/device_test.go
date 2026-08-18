package config

import (
	"encoding/json"
	"testing"
)

func TestNormaliseDevicesLegacyShorthand(t *testing.T) {
	devices := map[string]DeviceConfig{
		"glowsensorth1": {
			IEEEAddress:  "0x00124b00",
			FriendlyName: "Glow Sensor",
			Class:        "environmental_sensor",
		},
	}
	normaliseDevices(devices)
	d := devices["glowsensorth1"]
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
		"weatherstation": {
			Scheme:       "tasmota",
			Primary:      "tasmota-123",
			Display:      "Canonical Name",
			IEEEAddress:  "0xdead",
			FriendlyName: "Legacy Name",
		},
	}
	normaliseDevices(devices)
	d := devices["weatherstation"]
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
		"climate_basement": {Class: "environmental_sensor"},
	}
	normaliseDevices(devices)
	d := devices["climate_basement"]
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
		// office/utility they are the ONLY environment source.
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

// Floor is derived from the room id's "<floor>.<slug>" shape, and returns ""
// whenever the floor is genuinely unknown rather than guessing one.
func TestDeviceConfig_Floor(t *testing.T) {
	cases := []struct {
		name string
		dev  DeviceConfig
		want string
	}{
		{"room id", DeviceConfig{Room: "groundfloor.kitchen"}, "groundfloor"},
		{"hyphenated slug", DeviceConfig{Room: "firstfloor.drawing-room"}, "firstfloor"},
		{"nested slug takes the first segment", DeviceConfig{Room: "secondfloor.bath.ensuite"}, "secondfloor"},
		{"legacy free-text location has no floor", DeviceConfig{Location: "basement"}, ""},
		{"house sentinel resolves to no room, so no floor", DeviceConfig{Location: CoverageHouse}, ""},
		{"unplaced device", DeviceConfig{}, ""},
		{"leading dot is not a floor", DeviceConfig{Room: ".kitchen"}, ""},
		// Room wins over the deprecated Location, exactly as Place does.
		{"room beats location", DeviceConfig{Room: "thirdfloor.attic", Location: "loft"}, "thirdfloor"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.dev.Floor(); got != tc.want {
				t.Errorf("Floor() = %q, want %q", got, tc.want)
			}
		})
	}
}
