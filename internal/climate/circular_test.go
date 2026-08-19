package climate

import (
	"math"
	"testing"
	"time"

	"github.com/sweeney/greenhouse/internal/config"
)

// A circular field is an angular 0–360° quantity, and the arithmetic that is
// merely non-additive for a temperature is outright INVALID for a bearing:
// mean(350°, 10°) is 180° (due South) though both readings say North.
//
// ValidFnForField already refuses mean/min/max on the per-device axis. These
// tests pin the same refusal on the cross-member axis, which used to average
// bearings silently because the combine never saw the field.

const circularField = "wind_dir_deg"

// twoBucketAxis is a two-bucket axis; the values under test are per-bucket, so
// two is enough to show a gap and a reading side by side.
func twoBucketAxis() []time.Time {
	return []time.Time{time.Unix(0, 0).UTC(), time.Unix(3600, 0).UTC()}
}

// oneRoomTwoSensors puts two wind sensors in a single room, which is the
// configuration that makes the cross-member combine engage.
func oneRoomTwoSensors() map[string]config.DeviceConfig {
	return map[string]config.DeviceConfig{
		"probe_a": {
			Class: "environmental_sensor", Room: "floor1.room-a", Floor: "floor1",
			DisplayName: "Probe A",
		},
		"probe_b": {
			Class: "environmental_sensor", Room: "floor1.room-a", Floor: "floor1",
			DisplayName: "Probe B",
		},
	}
}

func seriesByKey(got []Series) map[string]Series {
	out := map[string]Series{}
	for _, s := range got {
		out[s.Key] = s
	}
	return out
}

// The regression this file exists for. Two sensors either side of North average
// to due South under the arithmetic mean; the combine must refuse instead.
func TestAssembleSeries_Circular_TwoMembersIsAGapNotDueSouth(t *testing.T) {
	got := AssembleSeries(twoBucketAxis(), oneRoomTwoSensors(), map[string][]float64{
		"probe_a": {350, 350},
		"probe_b": {10, 10},
	}, GroupByRoom, circularField, DefaultGroupFn)

	room, ok := seriesByKey(got)["floor1.room-a"]
	if !ok {
		t.Fatalf("no series for the room, got %v", got)
	}
	for i, v := range room.Values {
		if v == 180 {
			t.Fatalf("bucket %d combined 350° and 10° into 180° (due South) — "+
				"the arithmetic mean of a bearing is what this must never emit", i)
		}
		if !math.IsNaN(v) {
			t.Errorf("bucket %d = %v, want NaN: two members reported and there is "+
				"no defensible single bearing", i, v)
		}
	}
}

// The refusal is about member COUNT, not about the particular bearings: two
// members that happen to agree exactly are still two members, and averaging them
// would be luck rather than correctness.
func TestAssembleSeries_Circular_TwoAgreeingMembersStillGap(t *testing.T) {
	got := AssembleSeries(twoBucketAxis(), oneRoomTwoSensors(), map[string][]float64{
		"probe_a": {90, 90},
		"probe_b": {90, 90},
	}, GroupByRoom, circularField, DefaultGroupFn)

	room := seriesByKey(got)["floor1.room-a"]
	if !math.IsNaN(room.Values[0]) {
		t.Errorf("values[0] = %v, want NaN: agreement is not a licence to average",
			room.Values[0])
	}
}

// A single reporting member is always answerable: one instantaneous bearing is
// valid, and passing it through is not an average. This is the case that keeps
// the one-wind-sensor deployment working exactly as before.
func TestAssembleSeries_Circular_SingleMemberPassesThrough(t *testing.T) {
	got := AssembleSeries(twoBucketAxis(), oneRoomTwoSensors(), map[string][]float64{
		"probe_a": {350, 10},
		// probe_b reports nothing at all.
	}, GroupByRoom, circularField, DefaultGroupFn)

	room := seriesByKey(got)["floor1.room-a"]
	if room.Values[0] != 350 || room.Values[1] != 10 {
		t.Errorf("values = %v, want [350 10] passed through unchanged", room.Values)
	}
}

// The member count is evaluated PER BUCKET, not per series: a room where only
// one sensor was reporting early on and both later must answer for the first
// buckets and refuse for the rest.
func TestAssembleSeries_Circular_RefusalIsPerBucket(t *testing.T) {
	got := AssembleSeries(twoBucketAxis(), oneRoomTwoSensors(), map[string][]float64{
		"probe_a": {350, 350},
		"probe_b": {math.NaN(), 10}, // offline for bucket 0, reporting in bucket 1
	}, GroupByRoom, circularField, DefaultGroupFn)

	room := seriesByKey(got)["floor1.room-a"]
	if room.Values[0] != 350 {
		t.Errorf("values[0] = %v, want 350: only one member reported that bucket",
			room.Values[0])
	}
	if !math.IsNaN(room.Values[1]) {
		t.Errorf("values[1] = %v, want NaN: both members reported that bucket",
			room.Values[1])
	}
}

// A bucket nobody reported stays a gap, exactly as for a linear field. The
// circular rule adds a reason to gap; it never removes one.
func TestAssembleSeries_Circular_NoMembersIsStillAGap(t *testing.T) {
	got := AssembleSeries(twoBucketAxis(), oneRoomTwoSensors(), map[string][]float64{
		"probe_a": {math.NaN(), 10},
	}, GroupByRoom, circularField, DefaultGroupFn)

	room := seriesByKey(got)["floor1.room-a"]
	if !math.IsNaN(room.Values[0]) {
		t.Errorf("values[0] = %v, want NaN", room.Values[0])
	}
}

// group_by=device never combines members, so a circular field charts normally
// there. This is the escape hatch the 400 points callers at, so it must work.
func TestAssembleSeries_Circular_ByDeviceIsUnaffected(t *testing.T) {
	got := AssembleSeries(twoBucketAxis(), oneRoomTwoSensors(), map[string][]float64{
		"probe_a": {350, 350},
		"probe_b": {10, 10},
	}, GroupByDevice, circularField, DefaultGroupFn)

	byKey := seriesByKey(got)
	if v := byKey["probe_a"].Values; v[0] != 350 || v[1] != 350 {
		t.Errorf("probe_a values = %v, want [350 350]", v)
	}
	if v := byKey["probe_b"].Values; v[0] != 10 || v[1] != 10 {
		t.Errorf("probe_b values = %v, want [10 10]", v)
	}
}

// Min/Max/Mean are LINEAR statistics and are undefined on a circular axis, so
// they are null for circular fields however many members reported. A legend
// reading "min 10°, max 350°" describes a 20° spread as though it were 340°.
func TestAssembleSeries_Circular_SummaryStatsAreNull(t *testing.T) {
	got := AssembleSeries(twoBucketAxis(), oneRoomTwoSensors(), map[string][]float64{
		"probe_a": {350, 10},
	}, GroupByDevice, circularField, DefaultGroupFn)

	s := seriesByKey(got)["probe_a"]
	for name, v := range map[string]float64{"Min": s.Min, "Max": s.Max, "Mean": s.Mean} {
		if !math.IsNaN(v) {
			t.Errorf("%s = %v, want NaN: a linear summary of bearings is meaningless "+
				"(350° and 10° are 20° apart, not 340°)", name, v)
		}
	}
	// The VALUES are still real: refusing to summarise is not refusing to chart.
	if s.Values[0] != 350 || s.Values[1] != 10 {
		t.Errorf("values = %v, want the bearings themselves", s.Values)
	}
}

// Everything above must leave linear fields completely untouched — the fix is
// scoped to the circular flag, not to grouping in general.
func TestAssembleSeries_Linear_StillMeansAcrossMembers(t *testing.T) {
	got := AssembleSeries(twoBucketAxis(), oneRoomTwoSensors(), map[string][]float64{
		"probe_a": {20, 20},
		"probe_b": {22, 24},
	}, GroupByRoom, DefaultField, DefaultGroupFn)

	room := seriesByKey(got)["floor1.room-a"]
	if room.Values[0] != 21 || room.Values[1] != 22 {
		t.Errorf("values = %v, want [21 22]: the mean across members is unchanged "+
			"for a non-circular field", room.Values)
	}
	if math.IsNaN(room.Min) || math.IsNaN(room.Max) || math.IsNaN(room.Mean) {
		t.Errorf("summary stats = %v/%v/%v, want real numbers for a linear field",
			room.Min, room.Max, room.Mean)
	}
}

// An unrecognised field name is treated as non-circular: the registry is the
// authority on which fields are angular, and callers validate against it before
// assembly. Guessing from the name would be a second registry.
func TestAssembleSeries_UnknownFieldTreatedAsLinear(t *testing.T) {
	got := AssembleSeries(twoBucketAxis(), oneRoomTwoSensors(), map[string][]float64{
		"probe_a": {20, 20},
		"probe_b": {22, 24},
	}, GroupByRoom, "not_a_registered_field", DefaultGroupFn)

	room := seriesByKey(got)["floor1.room-a"]
	if room.Values[0] != 21 {
		t.Errorf("values[0] = %v, want 21", room.Values[0])
	}
}

// --- CircularGroupConflict ---

// The detector is what lets the API answer with an explanation rather than
// unexplained gaps, so it must agree with the combine about when members meet.
func TestCircularGroupConflict_DetectsASharedRoom(t *testing.T) {
	key, n, conflict := CircularGroupConflict(circularField, GroupByRoom, oneRoomTwoSensors())
	if !conflict {
		t.Fatal("two wind sensors in one room must be reported as a conflict")
	}
	if key != "floor1.room-a" {
		t.Errorf("key = %q, want the room that holds both", key)
	}
	if n != 2 {
		t.Errorf("n = %d, want 2", n)
	}
}

// One sensor per room is answerable: every group is a singleton, so nothing is
// ever averaged. This is today's fleet, and it must not start 400ing.
func TestCircularGroupConflict_SingletonRoomsAreFine(t *testing.T) {
	devices := map[string]config.DeviceConfig{
		"probe_a": {Class: "environmental_sensor", Room: "floor1.room-a"},
		"probe_b": {Class: "environmental_sensor", Room: "floor1.room-b"},
	}
	if _, _, conflict := CircularGroupConflict(circularField, GroupByRoom, devices); conflict {
		t.Error("one sensor per room combines nothing and must not conflict")
	}
}

// Class is applied first, exactly as it is for the filters: a non-climate device
// sharing the room is not a member, so it cannot create a conflict.
func TestCircularGroupConflict_IgnoresNonClimateDevices(t *testing.T) {
	devices := map[string]config.DeviceConfig{
		"probe_a": {Class: "environmental_sensor", Room: "floor1.room-a"},
		"plug_a":  {Class: "continuous_power_device", Room: "floor1.room-a"},
	}
	if _, _, conflict := CircularGroupConflict(circularField, GroupByRoom, devices); conflict {
		t.Error("a plug is not a climate member and must not create a conflict")
	}
}

// A device with no room id is UNKNOWN, not a group of its own, so two of them do
// not constitute a shared group — consistent with assembleByRoom omitting them.
func TestCircularGroupConflict_RoomlessDevicesAreNotAGroup(t *testing.T) {
	devices := map[string]config.DeviceConfig{
		"probe_a": {Class: "environmental_sensor"},
		"probe_b": {Class: "environmental_sensor"},
	}
	if _, _, conflict := CircularGroupConflict(circularField, GroupByRoom, devices); conflict {
		t.Error("room-less devices are not grouped together, so there is no conflict")
	}
}

// A linear field is combinable however many members share a group.
func TestCircularGroupConflict_LinearFieldNeverConflicts(t *testing.T) {
	if _, _, conflict := CircularGroupConflict(DefaultField, GroupByRoom, oneRoomTwoSensors()); conflict {
		t.Error("temperature across two sensors is a legitimate mean")
	}
}

// group_by=device gives every device its own series, so there is nothing to
// combine and nothing to reject.
func TestCircularGroupConflict_ByDeviceNeverConflicts(t *testing.T) {
	if _, _, conflict := CircularGroupConflict(circularField, GroupByDevice, oneRoomTwoSensors()); conflict {
		t.Error("group_by=device combines no members")
	}
}

// An unknown field cannot be known to be circular, so it does not conflict —
// matching the assembly step, which treats it as linear.
func TestCircularGroupConflict_UnknownFieldNeverConflicts(t *testing.T) {
	if _, _, conflict := CircularGroupConflict("nope", GroupByRoom, oneRoomTwoSensors()); conflict {
		t.Error("an unregistered field is not known to be circular")
	}
}

// The reported key is the lowest-sorting offender, so the same request always
// produces the same error message however Go happens to order the map.
func TestCircularGroupConflict_PicksTheLowestSortingGroup(t *testing.T) {
	devices := map[string]config.DeviceConfig{
		"probe_a": {Class: "environmental_sensor", Room: "floor2.room-z"},
		"probe_b": {Class: "environmental_sensor", Room: "floor2.room-z"},
		"probe_c": {Class: "environmental_sensor", Room: "floor1.room-a"},
		"probe_d": {Class: "environmental_sensor", Room: "floor1.room-a"},
	}
	for i := 0; i < 50; i++ {
		key, _, conflict := CircularGroupConflict(circularField, GroupByRoom, devices)
		if !conflict || key != "floor1.room-a" {
			t.Fatalf("iteration %d: key = %q (conflict=%v), want a stable floor1.room-a",
				i, key, conflict)
		}
	}
}

// Membership is DECLARED membership: a sensor that reported nothing in the window
// is still in the room, so the same request does not 400 today and 200 tomorrow
// because a sensor happened to be offline.
func TestCircularGroupConflict_CountsDeclaredMembersNotReportingOnes(t *testing.T) {
	// The detector sees only config — this test pins that contract by showing a
	// conflict for devices with no values supplied at all.
	if _, _, conflict := CircularGroupConflict(circularField, GroupByRoom, oneRoomTwoSensors()); !conflict {
		t.Error("a conflict is declared by config, not by which sensors happen to be up")
	}
}

// GroupKeyFor is the single definition of "which devices share a series", used
// by both the assembly step and the validation. If it ever disagrees with
// itself, readings get averaged together that should not be.
func TestGroupKeyFor(t *testing.T) {
	d := config.DeviceConfig{Class: "environmental_sensor", Room: "floor1.room-a"}
	if k := GroupKeyFor(GroupByRoom)(d); k != "floor1.room-a" {
		t.Errorf("room key = %q, want the room id", k)
	}
	if GroupKeyFor(GroupByDevice) != nil {
		t.Error("group_by=device must report nil: it never combines members")
	}
	if GroupKeyFor("nonsense") != nil {
		t.Error("an unknown grouping must report nil rather than a guess")
	}
}
