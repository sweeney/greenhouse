package climate

import (
	"testing"

	"github.com/sweeney/greenhouse/internal/config"
)

// An appliance_probe is charted like any climate device but describes an
// appliance INTERIOR, not the room it stands in. These tests pin the gap
// between the two predicates: a probe must reach group_by=device and must
// never reach a room or floor combine, because its room is necessarily the
// appliance's room and its setpoint is nothing like the ambient temperature.

// probeFixture puts a freezer probe in a kitchen that also holds a real ambient
// sensor. The two numbers are far apart on purpose: -19 against 20 means any
// leak of the probe into the room statistic is unmistakable rather than a
// rounding difference.
func probeFixture() map[string]config.DeviceConfig {
	return map[string]config.DeviceConfig{
		"climate_kitchen": {Class: "environmental_sensor", Floor: "groundfloor", Room: "groundfloor.kitchen"},
		"probe_freezer":   {Class: "appliance_probe", Floor: "groundfloor", Room: "groundfloor.kitchen"},
	}
}

func probeValues() map[string][]float64 {
	return map[string][]float64{
		"climate_kitchen": {20, 20},
		"probe_freezer":   {-19, -19},
	}
}

// The whole reason the class is split out. Averaging the probe in would report
// 0.5 °C for a kitchen sitting at 20 °C.
func TestAssembleSeries_ByRoom_ExcludesApplianceProbe(t *testing.T) {
	got := seriesByKey(AssembleSeries(
		twoBucketAxis(), probeFixture(), probeValues(), GroupByRoom, DefaultField, GroupFnMean, nil))

	if len(got) != 1 {
		t.Fatalf("want one room series, got %d: %v", len(got), got)
	}
	if v := got["groundfloor.kitchen"].Values[0]; v != 20 {
		t.Errorf("kitchen = %v, want 20 (the ambient sensor alone, not the mean with a -19 freezer probe)", v)
	}
}

// Same question one axis up: a floor mean must not drift toward a freezer
// either, and floor is a separate code path from room only in its key function.
func TestAssembleSeries_ByFloor_ExcludesApplianceProbe(t *testing.T) {
	got := seriesByKey(AssembleSeries(
		twoBucketAxis(), probeFixture(), probeValues(), GroupByFloor, DefaultField, GroupFnMean, nil))

	if v := got["groundfloor"].Values[0]; v != 20 {
		t.Errorf("groundfloor = %v, want 20 (probe excluded from the floor combine)", v)
	}
}

// min is the combine a stray freezer probe corrupts most violently, so it gets
// its own assertion rather than trusting that mean covers the family.
func TestAssembleSeries_ByRoom_ProbeDoesNotDragTheMinimum(t *testing.T) {
	got := seriesByKey(AssembleSeries(
		twoBucketAxis(), probeFixture(), probeValues(), GroupByRoom, DefaultField, GroupFnMin, nil))

	if v := got["groundfloor.kitchen"].Values[0]; v != 20 {
		t.Errorf("kitchen min = %v, want 20 — a -19 probe must not become the room's minimum", v)
	}
}

// Excluded from the combine, but NOT from greenhouse: group_by=device is how a
// probe is charted, and it must carry its own series with its own readings.
func TestAssembleSeries_ByDevice_ChartsApplianceProbe(t *testing.T) {
	got := seriesByKey(AssembleSeries(
		twoBucketAxis(), probeFixture(), probeValues(), GroupByDevice, DefaultField, "", nil))

	if len(got) != 2 {
		t.Fatalf("want a series per device including the probe, got %d: %v", len(got), got)
	}
	s, ok := got["probe_freezer"]
	if !ok {
		t.Fatalf("probe_freezer has no series; group_by=device is the ONLY way to chart a probe")
	}
	if v := s.Values[0]; v != -19 {
		t.Errorf("probe_freezer = %v, want -19 passed through untouched", v)
	}
	// It keeps the appliance's room — the probe genuinely is in the kitchen.
	// Excluding it from the room COMBINE is not the same as denying it a room.
	if s.Room != "groundfloor.kitchen" {
		t.Errorf("probe room = %q, want groundfloor.kitchen", s.Room)
	}
}

// A room holding ONLY probes has no ambient reading at all, so it is omitted
// rather than keyed on a freezer interior. Same rule as a device with no room:
// greenhouse does not invent a value nobody measured.
func TestAssembleSeries_ByRoom_ProbeOnlyRoomIsOmitted(t *testing.T) {
	devices := map[string]config.DeviceConfig{
		"probe_winecooler": {Class: "appliance_probe", Floor: "basement", Room: "basement.cellar"},
	}
	values := map[string][]float64{"probe_winecooler": {6, 6}}

	got := seriesByKey(AssembleSeries(
		twoBucketAxis(), devices, values, GroupByRoom, DefaultField, GroupFnMean, nil))

	if len(got) != 0 {
		t.Fatalf("want no room series for a probe-only room, got %v", got)
	}
}
