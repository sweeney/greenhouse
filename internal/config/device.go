package config

import "github.com/sweeney/common/devices"

// DeviceConfig is one device in the site's devices namespace: the shared
// schema in common/devices. Greenhouse reads Class, Room (via Place), Floor,
// DisplayName, TargetTemperature and EnvironmentFields.
//
// TargetTemperature is relayed as the number and nothing more: absent is
// UNKNOWN (null), never a guess from the class or name, and how far from
// target is acceptable is each client's policy, not a fact about the device.
//
// EnvironmentFields is an optional hint naming the fields a device writes to
// `device_environment`. When absent, greenhouse falls back to the full field
// registry (see MayReportField), so populating it is what makes /devices
// honest about what each sensor offers.
type DeviceConfig = devices.Device

// Thresholds are statehouse's activity thresholds, part of the shared schema.
// Greenhouse does not use them.
type Thresholds = devices.Thresholds

// CoverageHouse is the legacy `location` value meaning a device's readings
// describe the whole property rather than the room it sits in. Resolving it as
// a room would key a series on `house`, a reserved series key.
const CoverageHouse = devices.CoverageHouse

// climateClasses are the statehouse device classes whose members write
// environmental telemetry to the `device_environment` measurement, and which
// greenhouse therefore CHARTS.
//
// `fire_alarm` is here because the three installed alarms each write
// temperature_c alongside their smoke state — and crucially, some rooms
// hold NO environmental_sensor at all, so without them those two rooms have no
// climate coverage despite live data sitting in Influx.
//
// `appliance_probe` is here because the probes write temperature_c exactly as
// the TH sensors do — but what they describe is an appliance interior, not the
// air in the room, so it is NOT in ambientClasses below. See that map.
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
	"appliance_probe":      {},
}

// ambientClasses are the climate classes whose readings describe the AIR IN THE
// ROOM, and which may therefore be combined into a room or floor statistic.
//
// It is a subset of climateClasses, and the gap between the two is the whole
// point. An `appliance_probe` is a temperature probe potted in thermal ballast
// INSIDE a fridge, a freezer or a wine cooler: it reports temperature_c like any
// TH sensor, so greenhouse charts it, but it measures an appliance interior. The
// probe's room is necessarily the appliance's room, so a naive group_by=room
// over every climate device averages a -19 °C freezer interior into the kitchen
// and reports an ambient temperature no thermometer in that room would show.
// statehouse's own class comment (device/profiles.go, ClassApplianceProbe) names
// this as the reason the class exists: "Consumers aggregating ambient conditions
// by room need to exclude these."
//
// So the two predicates split the question that used to be one:
//
//	ReportsEnvironment — may this device be charted at all?  (catalog,
//	                       devices=, /devices/{id}/*, group_by=device)
//	DescribesAmbient   — may its reading be COMBINED with its neighbours'?
//	                       (group_by=room, group_by=floor, /rooms, /floors)
//
// This is NOT a per-package class const of the kind the allowlist replaced —
// both maps live here and are reached only through the two functions below, so
// the climate and httpapi packages still agree by construction.
//
// A class absent from climateClasses is not charted at all, so listing it here
// would say nothing; keep this map a subset.
var ambientClasses = map[string]struct{}{
	"environmental_sensor": {},
	"fire_alarm":           {},
}

// ReportsEnvironment reports whether greenhouse charts this device — i.e.
// whether its class writes to the `device_environment` measurement. It is the
// predicate behind the device catalog, the series device set, the
// devices=/rooms=/floors= filters and group_by=device.
//
// It answers "may this be charted", NOT "may this be averaged with its
// neighbours" — see DescribesAmbient, which is the narrower question.
func ReportsEnvironment(d DeviceConfig) bool {
	_, ok := climateClasses[d.Class]
	return ok
}

// DescribesAmbient reports whether this device's readings describe the air in
// the room it sits in, and may therefore be COMBINED into a room or floor
// statistic. It is the predicate behind group_by=room, group_by=floor and the
// /rooms and /floors catalogs.
//
// Every ambient device is also a charted one (ambientClasses is a subset of
// climateClasses), so callers never need both: use this one wherever two
// devices' readings are about to be averaged, and ReportsEnvironment
// everywhere else.
//
// The class it excludes is `appliance_probe`, whose readings are of an
// appliance interior rather than a room — averaging a freezer probe into its
// kitchen reports an ambient temperature nothing in that room would measure.
// See ambientClasses for the full reasoning.
func DescribesAmbient(d DeviceConfig) bool {
	_, ok := ambientClasses[d.Class]
	return ok
}

// MayReportField reports whether this device could plausibly have data for
// field, according to its declared EnvironmentFields.
//
// It is deliberately PERMISSIVE when coverage is unknown: a device declaring no
// EnvironmentFields returns true for every field. EnvironmentFields is config,
// and config can be stale — if a sensor starts reporting a new field before the
// namespace is updated, treating "undeclared" as "does not report" would turn a
// config oversight into a data outage, rejecting readings that genuinely exist.
// Callers therefore only ever narrow or reject on a POSITIVE declaration.
//
// This mirrors the catalog's registry fallback: no declaration means "we don't
// know", and greenhouse does not block on what it does not know.
func MayReportField(d DeviceConfig, field string) bool {
	if len(d.EnvironmentFields) == 0 {
		return true
	}
	for _, f := range d.EnvironmentFields {
		if f == field {
			return true
		}
	}
	return false
}
