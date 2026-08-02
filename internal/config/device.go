package config

import "time"

// Thresholds describes the per-class activity detection thresholds.
// Greenhouse does not use thresholds itself, but the struct is mirrored
// from statehouse so that DeviceConfig fetched from the shared
// `statehouse_devices` namespace round-trips cleanly. All fields are
// pointers so that an explicitly-set zero value is honoured.
type Thresholds struct {
	IdleBelowW           *float64       `yaml:"idle_below_w"            json:"-"`
	ActiveAboveW         *float64       `yaml:"active_above_w"          json:"-"`
	ActiveSustainedFor   *time.Duration `yaml:"active_sustained_for"    json:"-"`
	InactiveSustainedFor *time.Duration `yaml:"inactive_sustained_for"  json:"-"`
	CompressorAboveW     *float64       `yaml:"compressor_above_w"      json:"-"`
}

// DeviceConfig mirrors statehouse's device entry. Greenhouse reads only
// Class, Location, and DisplayName (to find climate devices and group series
// by location), but the full struct is kept so the shared `statehouse_devices`
// namespace parses without loss. The canonical identity fields are
// Scheme + Primary (and Display); the legacy `ieee_address` /
// `friendly_name` fields are Z2M shorthand that normaliseDevices folds in.
type DeviceConfig struct {
	// Canonical identity fields. Scheme names the adapter that owns the
	// device ("zigbee", "tasmota", "shelly", ...). Primary is the
	// adapter's stable identifier. Display is the human-readable name.
	Scheme  string `yaml:"scheme"   json:"scheme,omitempty"`
	Primary string `yaml:"primary"  json:"primary,omitempty"`
	Display string `yaml:"display"  json:"display,omitempty"`

	// Legacy Z2M shorthand. normaliseDevices converts these to
	// scheme=zigbee + primary=ieee_address / display=friendly_name.
	IEEEAddress  string `yaml:"ieee_address"   json:"ieee_address,omitempty"`
	FriendlyName string `yaml:"friendly_name"  json:"friendly_name,omitempty"`

	Class       string      `yaml:"class"            json:"class,omitempty"`
	DisplayName string      `yaml:"display_name"     json:"display_name,omitempty"`
	Location    string      `yaml:"location"         json:"location,omitempty"`
	Thresholds  *Thresholds `yaml:"thresholds"       json:"thresholds,omitempty"`

	// EnergyStrategy is mirrored for completeness so the shared namespace
	// round-trips; greenhouse never reads it (it is an energy concern).
	EnergyStrategy string `yaml:"energy_strategy" json:"energy_strategy,omitempty"`

	// EnvironmentFields, when present in the namespace, lists the fields this
	// device actually writes to the `device_environment` measurement (e.g.
	// ["humidity_pct","temperature_c"]). Named for that measurement rather
	// than a bare "fields": statehouse_devices is a SHARED namespace, so a
	// generic key would be ambiguous the moment a sibling service wants to
	// declare its own per-device field list.
	//
	// It is an optional hint. When absent, greenhouse falls back to the full
	// field registry for the device catalog, which over-advertises: an indoor
	// sensor reporting only temperature and humidity would appear to offer
	// rainfall and UV, and a series request for one of those returns 200 with
	// all-null buckets — indistinguishable from a sensor outage. Populating
	// this key in the namespace is what makes /devices honest.
	EnvironmentFields []string `yaml:"environment_fields" json:"environment_fields,omitempty"`
}

// climateClasses are the statehouse device classes whose members write
// environmental telemetry to the `device_environment` measurement, and which
// greenhouse therefore charts.
//
// `fire_alarm` is here because the three installed alarms each write
// temperature_c alongside their smoke state — and crucially, office and utility
// hold NO environmental_sensor at all, so without them those two rooms have no
// climate coverage despite live data sitting in Influx.
//
// KNOWN LIMITATION — this is a deliberate, documented trade-off:
//
// Selecting on class asserts "every device of this class reports environment
// telemetry". That is true of the current fleet but is not guaranteed: a future
// fire alarm model that does not report temperature would still be listed by
// /devices and would return a well-formed, permanently empty series. There is
// no way to detect that from config alone, and correcting it means editing this
// map and redeploying.
//
// The alternative — selecting on a non-empty EnvironmentFields — pushes the
// decision entirely into config, so a non-reporting device is simply omitted
// and no deploy is needed to onboard a new sensor class. It was NOT taken here
// because the current fleet is homogeneous and the class allowlist is a much
// smaller change; the option remains open and EnvironmentFields is already
// populated in the namespace for every reporting device, so switching later is
// a one-predicate change with no config migration.
//
// Adding a class here is a code change plus a deploy. Prefer that over
// scattering class comparisons: this map is the single source of truth, so the
// climate and httpapi packages agree by construction rather than by two
// consts that can drift apart.
var climateClasses = map[string]struct{}{
	"environmental_sensor": {},
	"fire_alarm":           {},
}

// ReportsEnvironment reports whether greenhouse charts this device — i.e.
// whether its class writes to the `device_environment` measurement. It is the
// single predicate behind the device catalog, the series device set, and the
// devices=/locations= filters.
func (d DeviceConfig) ReportsEnvironment() bool {
	_, ok := climateClasses[d.Class]
	return ok
}

// normaliseDevices converts legacy ieee_address/friendly_name shorthands
// into the canonical scheme/primary/display fields. Mirrors statehouse so
// devices fetched from the remote namespace are normalised identically.
func normaliseDevices(devices map[string]DeviceConfig) {
	for id, d := range devices {
		if d.Scheme == "" && (d.IEEEAddress != "" || d.FriendlyName != "") {
			d.Scheme = "zigbee"
		}
		if d.Primary == "" && d.IEEEAddress != "" {
			d.Primary = d.IEEEAddress
		}
		if d.Display == "" && d.FriendlyName != "" {
			d.Display = d.FriendlyName
		}
		devices[id] = d
	}
}
