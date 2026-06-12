package config

import "testing"

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
