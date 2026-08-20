package climate

import (
	"math"
	"testing"

	"github.com/sweeney/greenhouse/internal/config"
)

// Two independent axes:
//
//	per device:  fn=        collapses a device's samples within a bucket (Influx)
//	per group:   group_fn=  combines the group's devices              (here, Go)
//
// greenhouse used to expose only the first and hardcode the second to mean. These
// tests pin the second axis: the combines themselves, their interaction with
// gaps, and the fact that they do not commute with fn.

// floorFixture spans two floors. floor1 holds two rooms (three sensors between
// them) so a floor combine has something to combine that a room combine does not;
// floor2 holds one.
func floorFixture() map[string]config.DeviceConfig {
	return map[string]config.DeviceConfig{
		"sensor_e": {Class: "environmental_sensor", Floor: "floor1", Room: "floor1.room-a"},
		"sensor_f": {Class: "environmental_sensor", Floor: "floor1", Room: "floor1.room-a"},
		"sensor_g": {Class: "environmental_sensor", Floor: "floor1", Room: "floor1.room-b"},
		"sensor_h": {Class: "environmental_sensor", Floor: "floor2", Room: "floor2.room-a"},
	}
}

// floorValues gives each sensor a distinct constant so a mean, a min and a max
// over the floor are three different, recognisable numbers.
func floorValues() map[string][]float64 {
	return map[string][]float64{
		"sensor_e": {10, 10},
		"sensor_f": {20, 20},
		"sensor_g": {30, 30}, // floor1: mean 20, min 10, max 30
		"sensor_h": {5, 5},
	}
}

func assembleFloor(t *testing.T, groupFn string) map[string]Series {
	t.Helper()
	return seriesByKey(AssembleSeries(
		twoBucketAxis(), floorFixture(), floorValues(), GroupByFloor, DefaultField, groupFn, nil))
}

// --- group_by=floor ---

// A floor is the set of devices declaring it, across however many rooms. This is
// the grouping the README used to say greenhouse would not build.
func TestAssembleSeries_ByFloor_GroupsEveryRoomOnTheFloor(t *testing.T) {
	got := assembleFloor(t, GroupFnMean)

	if len(got) != 2 {
		t.Fatalf("want one series per floor, got %d: %v", len(got), got)
	}
	// floor1 spans two rooms and three sensors; the mean is over all three.
	if v := got["floor1"].Values[0]; v != 20 {
		t.Errorf("floor1 = %v, want the mean of 10, 20 and 30", v)
	}
	if v := got["floor2"].Values[0]; v != 5 {
		t.Errorf("floor2 = %v, want its single sensor's reading", v)
	}
}

// The floor is the DECLARED one, never read out of the room id's <floor>.<slug>
// shape — the invariant #21 established, now load-bearing for grouping too.
func TestAssembleSeries_ByFloor_TrustsTheDeclaredFloor(t *testing.T) {
	devices := map[string]config.DeviceConfig{
		// Declares floor3 while sitting in a room id that says floor1.
		"sensor_e": {Class: "environmental_sensor", Floor: "floor3", Room: "floor1.room-a"},
	}
	got := seriesByKey(AssembleSeries(twoBucketAxis(), devices,
		map[string][]float64{"sensor_e": {10, 10}}, GroupByFloor, DefaultField, GroupFnMean, nil))

	if _, ok := got["floor3"]; !ok {
		t.Errorf("want a series keyed on the declared floor3, got %v", got)
	}
	if _, ok := got["floor1"]; ok {
		t.Error("keyed on floor1: something derived a floor from the room id")
	}
}

// A floor-grouped series belongs to no single room, so it must not carry a floor
// id in a field named room.
func TestAssembleSeries_ByFloor_LeavesRoomEmpty(t *testing.T) {
	if room := assembleFloor(t, GroupFnMean)["floor1"].Room; room != "" {
		t.Errorf("room = %q, want empty: a floor is not a room", room)
	}
}

// Room grouping still carries its room, unchanged.
func TestAssembleSeries_ByRoom_StillCarriesRoom(t *testing.T) {
	got := seriesByKey(AssembleSeries(twoBucketAxis(), floorFixture(), floorValues(),
		GroupByRoom, DefaultField, GroupFnMean, nil))

	if room := got["floor1.room-a"].Room; room != "floor1.room-a" {
		t.Errorf("room = %q, want the room id", room)
	}
}

// The UNKNOWN case, settled identically on both axes: a device declaring no floor
// is omitted from a floor grouping, exactly as a room-less device is omitted from
// a room grouping. "" is not a valid id, and an invented "unknown" key would be a
// value floors=/rooms= reject — /series would advertise a vocabulary /series
// refuses.
func TestAssembleSeries_UnknownGroupKeyIsOmittedOnBothAxes(t *testing.T) {
	devices := map[string]config.DeviceConfig{
		"sensor_e": {Class: "environmental_sensor", Floor: "floor1", Room: "floor1.room-a"},
		"sensor_x": {Class: "environmental_sensor"}, // declares neither
	}
	vals := map[string][]float64{"sensor_e": {10, 10}, "sensor_x": {99, 99}}

	for _, groupBy := range []string{GroupByFloor, GroupByRoom} {
		t.Run(groupBy, func(t *testing.T) {
			got := seriesByKey(AssembleSeries(twoBucketAxis(), devices, vals, groupBy, DefaultField, GroupFnMean, nil))
			if len(got) != 1 {
				t.Fatalf("want exactly the one known group, got %v", got)
			}
			if _, ok := got[""]; ok {
				t.Error(`a series was keyed on "", which is not a valid id`)
			}
			for k, s := range got {
				if s.Values[0] == 99 {
					t.Errorf("group %q absorbed the unknown-membership device's reading", k)
				}
			}
		})
	}
}

// --- the combines ---

func TestAssembleSeries_GroupFn_Mean(t *testing.T) {
	if v := assembleFloor(t, GroupFnMean)["floor1"].Values[0]; v != 20 {
		t.Errorf("mean = %v, want 20", v)
	}
}

// min/max make the heterogeneity a mean hides visible: the floor's coldest room
// is the band's lower edge instead of disappearing into an average.
func TestAssembleSeries_GroupFn_Min(t *testing.T) {
	if v := assembleFloor(t, GroupFnMin)["floor1"].Values[0]; v != 10 {
		t.Errorf("min = %v, want the coldest member, 10", v)
	}
}

func TestAssembleSeries_GroupFn_Max(t *testing.T) {
	if v := assembleFloor(t, GroupFnMax)["floor1"].Values[0]; v != 30 {
		t.Errorf("max = %v, want the warmest member, 30", v)
	}
}

// Together the three render as a band with the mean through it, which is the
// shape that answers "a floor mean describes nowhere" honestly.
func TestAssembleSeries_GroupFn_MinMeanMaxFormABand(t *testing.T) {
	lo := assembleFloor(t, GroupFnMin)["floor1"].Values[0]
	mid := assembleFloor(t, GroupFnMean)["floor1"].Values[0]
	hi := assembleFloor(t, GroupFnMax)["floor1"].Values[0]

	if !(lo <= mid && mid <= hi) {
		t.Errorf("want min <= mean <= max, got %v/%v/%v", lo, mid, hi)
	}
	if lo == hi {
		t.Error("a floor spanning 10 to 30 must have a band with width")
	}
}

// An empty group_fn resolves to the default rather than falling through to some
// zero-valued combine.
func TestAssembleSeries_GroupFn_EmptyDefaultsToMean(t *testing.T) {
	got := seriesByKey(AssembleSeries(twoBucketAxis(), floorFixture(), floorValues(),
		GroupByFloor, DefaultField, "", nil))

	if v := got["floor1"].Values[0]; v != 20 {
		t.Errorf("empty group_fn = %v, want the mean 20", v)
	}
}

// Negative values are ordinary for temperature, and min/max must order them
// correctly rather than treating magnitude as size.
func TestAssembleSeries_GroupFn_HandlesNegatives(t *testing.T) {
	devices := map[string]config.DeviceConfig{
		"sensor_e": {Class: "environmental_sensor", Floor: "floor1"},
		"sensor_f": {Class: "environmental_sensor", Floor: "floor1"},
	}
	vals := map[string][]float64{"sensor_e": {-5, -5}, "sensor_f": {3, 3}}

	min := seriesByKey(AssembleSeries(twoBucketAxis(), devices, vals, GroupByFloor, DefaultField, GroupFnMin, nil))
	max := seriesByKey(AssembleSeries(twoBucketAxis(), devices, vals, GroupByFloor, DefaultField, GroupFnMax, nil))

	if v := min["floor1"].Values[0]; v != -5 {
		t.Errorf("min = %v, want -5", v)
	}
	if v := max["floor1"].Values[0]; v != 3 {
		t.Errorf("max = %v, want 3", v)
	}
}

// --- gaps ---

// Every combine SKIPS a member that did not report, exactly as the mean always
// did. A min that counted a gap as zero would report a freezing floor.
func TestAssembleSeries_GroupFn_SkipsNonReportingMembers(t *testing.T) {
	devices := map[string]config.DeviceConfig{
		"sensor_e": {Class: "environmental_sensor", Floor: "floor1"},
		"sensor_f": {Class: "environmental_sensor", Floor: "floor1"},
	}
	vals := map[string][]float64{
		"sensor_e": {20, 20},
		"sensor_f": {math.NaN(), math.NaN()}, // offline throughout
	}

	for _, fn := range []string{GroupFnMean, GroupFnMin, GroupFnMax} {
		t.Run(fn, func(t *testing.T) {
			got := seriesByKey(AssembleSeries(twoBucketAxis(), devices, vals, GroupByFloor, DefaultField, fn, nil))
			v := got["floor1"].Values[0]
			if v == 0 {
				t.Fatalf("%s treated an absent member as 0 — a gap is not a reading", fn)
			}
			if v != 20 {
				t.Errorf("%s = %v, want 20 from the only reporting member", fn, v)
			}
		})
	}
}

// A bucket nobody reported stays a gap for every combine, never a zero.
func TestAssembleSeries_GroupFn_EmptyBucketStaysAGap(t *testing.T) {
	devices := map[string]config.DeviceConfig{
		"sensor_e": {Class: "environmental_sensor", Floor: "floor1"},
	}
	vals := map[string][]float64{"sensor_e": {math.NaN(), 20}}

	for _, fn := range []string{GroupFnMean, GroupFnMin, GroupFnMax} {
		t.Run(fn, func(t *testing.T) {
			got := seriesByKey(AssembleSeries(twoBucketAxis(), devices, vals, GroupByFloor, DefaultField, fn, nil))
			if v := got["floor1"].Values[0]; !math.IsNaN(v) {
				t.Errorf("%s: empty bucket = %v, want NaN", fn, v)
			}
			if v := got["floor1"].Values[1]; v != 20 {
				t.Errorf("%s: reported bucket = %v, want 20", fn, v)
			}
		})
	}
}

// Membership is per bucket: a member that drops out mid-window changes the
// combine from that bucket on, rather than being excluded from the whole series.
func TestAssembleSeries_GroupFn_MembershipIsPerBucket(t *testing.T) {
	devices := map[string]config.DeviceConfig{
		"sensor_e": {Class: "environmental_sensor", Floor: "floor1"},
		"sensor_f": {Class: "environmental_sensor", Floor: "floor1"},
	}
	vals := map[string][]float64{
		"sensor_e": {10, 10},
		"sensor_f": {30, math.NaN()}, // present for bucket 0, gone for bucket 1
	}
	got := seriesByKey(AssembleSeries(twoBucketAxis(), devices, vals, GroupByFloor, DefaultField, GroupFnMax, nil))

	if v := got["floor1"].Values[0]; v != 30 {
		t.Errorf("bucket 0 max = %v, want 30 while both reported", v)
	}
	if v := got["floor1"].Values[1]; v != 10 {
		t.Errorf("bucket 1 max = %v, want 10 once only one reported", v)
	}
}

// --- interactions ---

// group_by=device never combines members, so group_fn cannot change its output.
// This is why an explicit group_fn there is a client error rather than a no-op.
func TestAssembleSeries_GroupFn_IrrelevantToGroupByDevice(t *testing.T) {
	var first []Series
	for _, fn := range []string{GroupFnMean, GroupFnMin, GroupFnMax} {
		got := AssembleSeries(twoBucketAxis(), floorFixture(), floorValues(),
			GroupByDevice, DefaultField, fn, nil)
		if first == nil {
			first = got
			continue
		}
		if len(got) != len(first) {
			t.Fatalf("group_fn=%s changed the series count", fn)
		}
		for i := range got {
			if got[i].Key != first[i].Key || got[i].Values[0] != first[i].Values[0] {
				t.Errorf("group_fn=%s changed per-device output: %v vs %v",
					fn, got[i], first[i])
			}
		}
	}
}

// A circular field is refused across members whatever the group_fn: min and max
// of a bearing are as meaningless as the mean, so no combine unlocks it.
func TestAssembleSeries_GroupFn_CircularStillRefusedForEveryCombine(t *testing.T) {
	for _, fn := range []string{GroupFnMean, GroupFnMin, GroupFnMax} {
		t.Run(fn, func(t *testing.T) {
			got := seriesByKey(AssembleSeries(twoBucketAxis(), oneRoomTwoSensors(),
				map[string][]float64{"probe_a": {350, 350}, "probe_b": {10, 10}},
				GroupByRoom, circularField, fn, nil))

			if v := got["floor1.room-a"].Values[0]; !math.IsNaN(v) {
				t.Errorf("group_fn=%s produced %v for a two-member bearing; no combine "+
					"makes an angular axis linear", fn, v)
			}
		})
	}
}

// The summary stats are computed over the COMBINED series, so they follow the
// group_fn rather than the raw member readings.
func TestAssembleSeries_GroupFn_SummaryStatsFollowTheCombine(t *testing.T) {
	devices := map[string]config.DeviceConfig{
		"sensor_e": {Class: "environmental_sensor", Floor: "floor1"},
		"sensor_f": {Class: "environmental_sensor", Floor: "floor1"},
	}
	vals := map[string][]float64{"sensor_e": {10, 10}, "sensor_f": {30, 30}}

	min := seriesByKey(AssembleSeries(twoBucketAxis(), devices, vals, GroupByFloor, DefaultField, GroupFnMin, nil))
	max := seriesByKey(AssembleSeries(twoBucketAxis(), devices, vals, GroupByFloor, DefaultField, GroupFnMax, nil))

	if m := min["floor1"].Mean; m != 10 {
		t.Errorf("group_fn=min series mean = %v, want 10: the summary describes the "+
			"combined line, not the members", m)
	}
	if m := max["floor1"].Mean; m != 30 {
		t.Errorf("group_fn=max series mean = %v, want 30", m)
	}
}

// --- the registry helpers ---

func TestValidGroupFn(t *testing.T) {
	for _, ok := range []string{GroupFnMean, GroupFnMin, GroupFnMax} {
		if !ValidGroupFn(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	// last is valid for fn= but not across members: "whichever sensor reported
	// most recently" is not a spatial statistic.
	for _, bad := range []string{"last", "sum", "median", "MEAN", "", " mean"} {
		if ValidGroupFn(bad) {
			t.Errorf("%q should not be valid", bad)
		}
	}
}

// sum stays absent on this axis too: climate is non-additive however it is sliced.
func TestGroupFns_ExcludesSumAndLast(t *testing.T) {
	got := GroupFns()
	want := []string{"max", "mean", "min"}
	if len(got) != len(want) {
		t.Fatalf("GroupFns() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("GroupFns()[%d] = %q, want %q (sorted)", i, got[i], want[i])
		}
	}
}

// Groups is what decides whether group_fn is meaningful, so it must agree with
// GroupKeyFor rather than being a second list of grouping modes.
func TestGroups(t *testing.T) {
	for _, g := range []string{GroupByRoom, GroupByFloor} {
		if !Groups(g) {
			t.Errorf("%q combines members and must report true", g)
		}
	}
	for _, g := range []string{GroupByDevice, "", "nonsense"} {
		if Groups(g) {
			t.Errorf("%q combines nothing and must report false", g)
		}
	}
}

func TestGroupKeyFor_Floor(t *testing.T) {
	d := config.DeviceConfig{Class: "environmental_sensor", Floor: "floor2", Room: "floor1.room-a"}
	if k := GroupKeyFor(GroupByFloor)(d); k != "floor2" {
		t.Errorf("floor key = %q, want the DECLARED floor2, not the room id's prefix", k)
	}
}
