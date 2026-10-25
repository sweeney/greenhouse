package climate

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/sweeney/greenhouse/internal/config"
	"github.com/sweeney/greenhouse/internal/influx"
)

// group_by modes for BuildSeries / AssembleSeries. There is NO "house" or
// "total" group: climate is non-additive, so there is nothing to total.
const (
	GroupByDevice = "device"
	// GroupByRoom groups by floorplan room id.
	GroupByRoom = "room"
	// GroupByFloor groups by the floor a device DECLARES (config.DeviceConfig.Floor),
	// never one derived from the room id. It is the coarse sibling of GroupByRoom,
	// and like it combines its members with the caller's GroupFn.
	GroupByFloor = "floor"
)

// group_fn modes: how a group's MEMBERS are combined into one series, once fn=
// has already collapsed each device's samples within a bucket.
//
// The two axes are independent and DO NOT COMMUTE. fn runs first, inside Influx
// (aggregateWindow, per device); group_fn runs second, here in Go (across the
// group's devices). So fn=mean&group_fn=max is "the warmest member's bucket
// average" and fn=max&group_fn=mean is "the mean of each member's peak" — both
// legitimate, and different numbers.
//
// There is no sum: climate is non-additive. There is no last either — "whichever
// sensor reported most recently" is not a spatial statistic, so it is rejected
// rather than answered arbitrarily.
const (
	GroupFnMean = "mean"
	GroupFnMin  = "min"
	GroupFnMax  = "max"
)

// DefaultGroupFn is the member combine applied when the caller omits group_fn.
// Mean is what greenhouse always did, so omitting group_fn is unchanged behaviour.
const DefaultGroupFn = GroupFnMean

// groupFns is the set of accepted group_fn values.
var groupFns = map[string]struct{}{
	GroupFnMean: {},
	GroupFnMin:  {},
	GroupFnMax:  {},
}

// ValidGroupFn reports whether g is an accepted member combine. The empty string
// is NOT valid; callers default to DefaultGroupFn before validating.
func ValidGroupFn(g string) bool {
	_, ok := groupFns[g]
	return ok
}

// GroupFns returns the accepted member combines, sorted for stable output.
func GroupFns() []string {
	out := make([]string, 0, len(groupFns))
	for g := range groupFns {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

// Groups reports whether groupBy combines multiple devices into one series.
// group_by=device gives each device its own line, so it never does.
func Groups(groupBy string) bool { return GroupKeyFor(groupBy) != nil }

// ValueDP is the decimal precision climate values are rounded to at the response
// boundary. Two places is plenty for °C/%/hPa/m/s/lux/index gauges and keeps
// payloads tidy. It is the single source of truth for "how greenhouse rounds";
// the httpapi layer rounds through RoundValue rather than redeclaring it.
const ValueDP = 2

// valueDP is the unexported alias used by this package's internal rounding.
const valueDP = ValueDP

// Series is one line in a SeriesResponse: a labelled, location-tagged set of
// per-bucket values aligned to the shared Buckets axis. Values has length
// len(buckets); a bucket with no reading is NaN, which marshals to JSON null
// (see MarshalJSON) so consumers can render gaps rather than a misleading 0.
//
// There are no totals: climate is non-additive. Min/Max/Mean summarise the
// series over the window for legends (NaN buckets are ignored).
type Series struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	// Room is the floorplan room id this series belongs to.
	Room string `json:"room,omitempty"`

	Values []float64 `json:"values"`

	Min  float64 `json:"min"`
	Max  float64 `json:"max"`
	Mean float64 `json:"mean"`
}

// seriesJSON is the wire form of Series with Values as []*float64 so NaN gaps
// marshal to null and the summary stats drop to null when the series is empty.
type seriesJSON struct {
	Key    string     `json:"key"`
	Label  string     `json:"label"`
	Room   string     `json:"room,omitempty"`
	Values []*float64 `json:"values"`
	Min    *float64   `json:"min"`
	Max    *float64   `json:"max"`
	Mean   *float64   `json:"mean"`
}

// MarshalJSON renders NaN values (empty buckets / empty-series stats) as JSON
// null, keeping the Go-side type a plain []float64 (per the package contract)
// while emitting valid JSON.
func (s Series) MarshalJSON() ([]byte, error) {
	out := seriesJSON{
		Key:    s.Key,
		Label:  s.Label,
		Room:   s.Room,
		Values: make([]*float64, len(s.Values)),
		Min:    nullable(s.Min),
		Max:    nullable(s.Max),
		Mean:   nullable(s.Mean),
	}
	for i, v := range s.Values {
		out.Values[i] = nullable(v)
	}
	return json.Marshal(out)
}

// nullable returns nil for NaN/Inf and a pointer to v otherwise.
func nullable(v float64) *float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}

// SeriesResponse is the columnar ("wide") time-series payload: a single shared
// time axis (Buckets) plus per-series value arrays that all align to it. This
// maps directly onto web charting libraries (one array per dataset) and is the
// default. For the row-oriented ("tidy"/long) alternative, see Rows().
type SeriesResponse struct {
	Window   string `json:"window"`
	From     string `json:"from"`
	To       string `json:"to"`
	Interval string `json:"interval"`
	GroupBy  string `json:"group_by"`
	Field    string `json:"field"`
	Unit     string `json:"unit"`
	Fn       string `json:"fn"`
	// GroupFn is the member combine that was applied, echoed so a caller can see
	// which question was answered rather than assuming the default. Omitted when
	// the grouping combines nothing (group_by=device).
	GroupFn string      `json:"group_fn,omitempty"`
	Shape   string      `json:"shape"` // "columns"
	Buckets []time.Time `json:"buckets"`
	Series  []Series    `json:"series"`
}

// fixedAxisStart returns the first canonical bucket start for a ≤1h interval:
// start snapped DOWN onto the absolute grid. Truncate rounds down to a multiple
// of iv.Duration since Go's zero time, which is a UTC midnight, so for every
// interval that divides an hour this is the UTC grid influx.windowLocation
// asks Influx to window on (issue #36).
//
// Snapping is what keeps the Go-owned axis on the same boundaries as Influx,
// whose timeSrc:"_start" stamps land on the grid regardless of where the query
// range begins. An UNSNAPPED axis stepping straight off a caller-derived start
// had boundaries Influx could never exact-match, and resolveBucket's snap-back
// rule — correct for a right-edge or interior stamp — could not tell those
// apart from an Influx LEFT edge falling inside a Go bucket. Every row landed in
// the preceding bucket and the whole series was reported one bucket early. That
// hit every "<N>h" rolling window and any custom `from` off the grid, at every
// sub-day interval. See issue #18.
//
// It used to anchor at the LOCAL MIDNIGHT of start's date. For ≤1h in a
// whole-hour zone (config refuses any other) that is the same grid, so labels
// are unchanged; the absolute grid just says so without a zone in the way. 6h
// no longer comes through here at all — it is a wall-clock interval
// (walkAxis), which is what removed the old known caveat that a fixed 6h step
// drifted an hour off the local grid after a clock change.
//
// Accepted trade: for an off-grid window the first bucket widens back to its
// grid boundary, so it is labelled a little before win.Start. That slice carries
// no in-window data — the query range still starts at win.Start — and the
// alternative is the shifted axis above. Countinghouse made the same trade.
func fixedAxisStart(start time.Time, iv Interval) time.Time {
	return start.Truncate(iv.Duration)
}

// BucketStarts returns the canonical local-timezone bucket-start axis for win
// at iv: the ascending list of bucket starts covering win.Start up to (but not
// including) win.Stop. This axis is the single source of truth every series
// aligns to; Influx results are demuxed onto it and gaps are NaN-filled.
//
// The axis is built three ways, one per kind of interval, and each matches the
// grid its readings are actually windowed on (issue #36; see Interval):
//
//   - ≤1h (5m/15m/30m/1h) is the ABSOLUTE grid: fixedAxisStart, then stepped by
//     iv.Duration. That is the UTC grid the query windows on, so every row's
//     _start stamp exact-matches a bucket. On the autumn changeover day the
//     repeated hour is two distinct sets of buckets (01:00 BST and 01:00 GMT)
//     and the day has 25 hours of them; in spring the missing hour simply does
//     not appear and the day has 23. The query used to ask Influx for LOCAL
//     wall-clock windows, which stretch across the repeated hour (the 01:45 BST
//     window at 15m ran to 02:00 GMT), so the buckets inside it came back null.
//     The query moved, not this axis.
//   - Wall-clock (6h) steps LOCAL wall-clock hours from local midnight, so the
//     buckets are 00/06/12/18 local on both sides of a change and the bucket
//     that straddles one is 7h (autumn) or 5h (spring). It used to step a fixed
//     6h, which drifted an hour off that grid after either change (autumn:
//     00:00+01:00, 05:00Z, 11:00Z, 17:00Z). Its readings are queried at 1h and
//     folded onto this axis (see BuildSeries).
//   - Calendar (1d) steps by CALENDAR day in loc, so a London day that is 23h
//     (spring forward) or 25h (autumn back) is still a single bucket starting
//     at local midnight. Influx's location-aware aggregateWindow stretches its
//     daily windows the same way.
//
// The 1d and 6h boundaries are INTENDED wall times, each resolved into loc on
// its own (walkAxis, resolveWall). A boundary a clock change skips (a zone whose
// clocks go forward at local midnight, like America/Santiago) starts at the
// transition instant, and an ambiguous one at its first occurrence; neither can
// shift the boundaries after it.
//
// countBuckets in interval.go computes this axis's LENGTH without materializing
// an unbounded one, to guard MaxBuckets cheaply. The two must agree exactly for
// any in-cap window; both derive the first ≤1h bucket from fixedAxisStart and
// walk the 1d/6h boundaries with walkAxis, so they cannot drift apart.
//
// Returned times are in loc, so they serialise with the local offset.
func BucketStarts(win Window, iv Interval, loc *time.Location) []time.Time {
	if loc == nil {
		loc = time.UTC
	}
	var out []time.Time
	if iv.Calendar || iv.wallClock() {
		walkAxis(win, iv, loc, func(b time.Time) bool {
			out = append(out, b)
			return true
		})
		return out
	}
	for cur := fixedAxisStart(win.Start, iv).In(loc); cur.Before(win.Stop); cur = cur.Add(iv.Duration) {
		out = append(out, cur)
	}
	return out
}

// walkAxis visits, in order, the bucket starts of a calendar (1d) or wall-clock
// (6h) axis for win in loc, until visit returns false or the next start is not
// before win.Stop. It is the one stepping behind both BucketStarts and
// countBuckets for those intervals.
//
// The first boundary is the wall-clock boundary at or before win.Start: local
// midnight of its date for 1d, the containing 00/06/12/18 for 6h. Hour() reads
// the wall clock, so a start inside the repeated hour (either occurrence of
// 01:xx) snaps to 00:00 local.
//
// Each INTENDED boundary is resolved on its own, never stepped from the previous
// resolved instant (countinghouse PR #50 review). Stepping from the resolved
// instant carried any resolution error forward for the rest of the window:
//
//   - 6h, America/Santiago 2026-09-06 (00:00 → 01:00): the missing 00:00
//     resolved to 23:00 the day before, and every later step inherited the hour,
//     leaving the axis on 05/11/17/23 for good.
//   - 1d, same day: the missing midnight resolved into the 5th, so "the next
//     day's midnight" was the 6th's again, the same instant came back, and the
//     loop never ended — hanging any /series request across the change.
//
// The "not after the previous" guard is belt and braces: resolveWall maps each
// intended boundary to an instant no earlier than the previous one, so it never
// fires for a real zone, but a loop driven by tzdata must not be able to spin.
func walkAxis(win Window, iv Interval, loc *time.Location, visit func(time.Time) bool) {
	start := win.Start.In(loc)
	var w time.Time
	var next func(time.Time) time.Time
	if iv.Calendar {
		w = wallTime(start.Year(), start.Month(), start.Day(), 0)
		next = func(w time.Time) time.Time { return w.AddDate(0, 0, 1) }
	} else {
		step := int(iv.Duration / time.Hour)
		w = wallTime(start.Year(), start.Month(), start.Day(), start.Hour()-start.Hour()%step)
		next = func(w time.Time) time.Time { return w.Add(iv.Duration) }
	}
	var prev time.Time
	for ; ; w = next(w) {
		cur := resolveWall(w, loc)
		if !cur.Before(win.Stop) {
			return
		}
		if !prev.IsZero() && !cur.After(prev) {
			continue // a boundary swallowed by a clock change: no empty bucket
		}
		if !visit(cur) {
			return
		}
		prev = cur
	}
}

// wallTime is a LOCAL wall-clock reading (year, month, day, hour) held as a
// zone-free value: a time.Time in UTC used purely as a calendar, so that adding
// hours or days to it is plain wall-clock arithmetic with no clock change in the
// way. resolveWall turns it into a real instant in a zone.
func wallTime(year int, month time.Month, day, hour int) time.Time {
	return time.Date(year, month, day, hour, 0, 0, 0, time.UTC)
}

// resolveWall returns the instant at which the local wall time w (see
// wallTime) begins in loc.
//
//   - An ordinary wall time is just itself.
//   - An AMBIGUOUS one (the repeated hour after a clock goes back) is its FIRST
//     occurrence, which is what time.Date returns and is when that wall time
//     first begins — so a day whose midnight repeats starts at the first.
//   - A wall time that does NOT EXIST (skipped when a clock goes forward) is the
//     transition instant: the boundary is "the moment the clock reached this
//     time", and on a spring-forward day the clock reaches 00:00 by jumping past
//     it. time.Date leaves the choice of side unspecified (it currently picks
//     the instant just BEFORE the transition, which reads as the previous wall
//     hour), so the side it picked is detected and the zone boundary on that
//     side is taken.
func resolveWall(w time.Time, loc *time.Location) time.Time {
	t := time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), 0, 0, 0, loc)
	got := wallTime(t.Year(), t.Month(), t.Day(), t.Hour())
	switch {
	case got.Equal(w):
		return t
	case got.Before(w):
		// Resolved short of the gap, in the zone period that ends at it.
		_, end := t.ZoneBounds()
		return end
	default:
		// Resolved past the gap, in the zone period that starts at it.
		start, _ := t.ZoneBounds()
		return start
	}
}

// RoundValue rounds a climate value to the standard display precision (ValueDP),
// passing NaN/Inf through untouched so empty buckets stay gaps. It is the one
// rounding entry point callers outside this package (e.g. httpapi) should use,
// so precision stays in lock-step with the values AssembleSeries pre-rounds.
func RoundValue(x float64) float64 { return roundTo(x, ValueDP) }

// roundTo rounds x to n decimal places (half away from zero). NaN/Inf pass
// through untouched so empty buckets stay gaps.
func roundTo(x float64, n int) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return x
	}
	p := math.Pow10(n)
	return math.Round(x*p) / p
}

// AssembleSeries is the PURE assembly step: given the canonical bucket axis, the
// device inventory and per-device per-bucket field values (already aligned to
// buckets, with NaN for gaps), it produces the grouped, NaN-preserving, rounded
// []Series.
//
// valueByDevice maps id→[]float64 aligned to buckets (len == len(buckets)); a
// missing device or a nil/short slice is treated as all-NaN (no data). Every
// emitted series has Values of length len(buckets).
//
// Grouping rules:
//   - device (default): one series per environmental device. key=id,
//     label=DisplayName, location carried through.
//   - room: members sharing a room are combined per groupFn (NON-additive — never
//     a sum). A bucket where no member reported stays NaN.
//   - floor: the same, over the floor each device DECLARES. Devices declaring no
//     room (room) or no floor (floor) are OMITTED rather than keyed on "" — see
//     assembleByGroup.
//
// groupFn selects the member combine (mean/min/max, defaulting to
// DefaultGroupFn when empty). It runs AFTER fn, which has already collapsed each
// device's samples within a bucket inside Influx; the two do not commute. It is
// irrelevant to group_by=device, which never combines members.
//
// groupLabels maps a group key to its human-readable display name, for grouped
// modes only. A key with no entry, or an entry that is empty, keeps the key
// itself as the label: greenhouse relays the floorplan's names and falls back to
// the id rather than deriving a label from it. Key is ALWAYS the id — only Label
// varies — so a caller matching on identity is unaffected by whether a name
// happens to be published. Per-device series take their label from DisplayName
// and ignore this entirely.
//
// field names the measurement being assembled, and is required because the
// combine is not field-agnostic: a CIRCULAR field (wind direction) cannot be
// arithmetically averaged across members — mean(350°, 10°) is 180° (South) when
// both readings say North. For a circular field a bucket with a single reporting
// member passes that member's bearing through unchanged (one instantaneous
// bearing is always valid), and a bucket where two or more members reported is a
// GAP: there is no defensible single bearing, so this function refuses to invent
// one. The same reason makes the linear Min/Max/Mean summary undefined on a
// circular axis, so those are NaN for circular fields too.
//
// An unknown field name is treated as non-circular — the registry is the
// authority and callers validate against it before reaching here.
func AssembleSeries(
	buckets []time.Time,
	devices map[string]config.DeviceConfig,
	valueByDevice map[string][]float64,
	groupBy string,
	field string,
	groupFn string,
	groupLabels map[string]string,
) []Series {
	n := len(buckets)
	get := func(id string) []float64 {
		v := valueByDevice[id]
		out := make([]float64, n)
		for i := range out {
			if i < len(v) {
				out[i] = v[i]
			} else {
				out[i] = math.NaN()
			}
		}
		return out
	}

	f, _ := FieldFor(field)
	circular := f.Circular

	if groupFn == "" {
		groupFn = DefaultGroupFn
	}
	if Groups(groupBy) {
		return assembleByGroup(buckets, devices, get, circular, groupBy, groupFn, groupLabels)
	}
	// device, and any unknown grouping, fall back to per-device.
	return assembleByDevice(buckets, devices, get, circular)
}

// assembleByDevice yields one series per environmental device, sorted by id.
//
// A per-device series has exactly one member, so the cross-member combine never
// engages: circular only affects the summary stats here (see buildSeries).
func assembleByDevice(
	buckets []time.Time,
	devices map[string]config.DeviceConfig,
	get func(string) []float64,
	circular bool,
) []Series {
	var out []Series
	for _, id := range sortedDeviceIDs(devices) {
		d := devices[id]
		if !d.ReportsEnvironment() {
			continue
		}
		label := d.DisplayName
		if label == "" {
			label = id
		}
		out = append(out, buildSeries(id, label, d.Place(), buckets, [][]float64{get(id)}, circular, DefaultGroupFn))
	}
	return out
}

// GroupKeyFor returns the function mapping a device to its series key under
// groupBy, or nil when groupBy gives every device its own series (group_by=device)
// and so never combines members.
//
// One definition of "which devices share a series", used by both the assembly
// step and the up-front validation in httpapi. Two implementations of that
// question would drift, and the thing that drifts is which readings get averaged
// together — a silent, wrong answer rather than a loud one.
func GroupKeyFor(groupBy string) func(config.DeviceConfig) string {
	switch groupBy {
	case GroupByRoom:
		return config.DeviceConfig.Place
	case GroupByFloor:
		// The DECLARED floor, never one read out of the room id's <floor>.<slug>
		// shape: the floorplan owns that fact (see config.DeviceConfig.Floor).
		return func(d config.DeviceConfig) string { return d.Floor }
	default:
		return nil
	}
}

// CircularGroupConflict reports whether groupBy would combine two or more of
// these devices' readings of field into one series, when field is circular and
// therefore cannot be combined at all (see buildSeries).
//
// It returns the offending group key and its member count, choosing the
// lowest-sorting key so the error message is deterministic. A non-circular
// field, a grouping that never combines members, or a grouping whose every group
// is a singleton all report false.
//
// Devices are counted by their DECLARED membership, not by whether they have
// data in the requested window: a request is rejected for what it asks for, so
// the same request does not 400 today and 200 tomorrow because a sensor was
// offline.
func CircularGroupConflict(field, groupBy string, devices map[string]config.DeviceConfig) (string, int, bool) {
	f, ok := FieldFor(field)
	if !ok || !f.Circular {
		return "", 0, false
	}
	keyOf := GroupKeyFor(groupBy)
	if keyOf == nil {
		return "", 0, false
	}
	counts := map[string]int{}
	for _, d := range devices {
		// Ambient, not merely charted: this counts the members a group would
		// COMBINE, and an appliance probe is never combined (DescribesAmbient).
		// Counting one would 400 a request that group_by is about to answer
		// with a single bearing passed through untouched.
		if !d.DescribesAmbient() {
			continue
		}
		if k := keyOf(d); k != "" {
			counts[k]++
		}
	}
	worst, n := "", 0
	for k, c := range counts {
		if c < 2 {
			continue
		}
		if worst == "" || k < worst {
			worst, n = k, c
		}
	}
	return worst, n, worst != ""
}

// assembleByGroup yields one series per distinct non-empty group key (room or
// floor) over environmental devices, combining member readings per groupFn.
//
// Climate is non-additive: the combine is mean/min/max across the group's
// sensors, never a total.
//
// UNKNOWN membership is OMITTED, and this is deliberate rather than inherited.
// A device declaring no room (group_by=room) or no floor (group_by=floor) has an
// empty key, and greenhouse neither keys a series on "" nor invents an "unknown"
// bucket for it: "" is not a valid room or floor id, and an invented key would be
// a value that rooms=/floors= reject, so /series would advertise a vocabulary
// /series itself refuses. Such a device is charted by group_by=device. The README
// says so, and TestSeries_FloorsFilter_GroupByRoomOmitsARoomlessDevice pins it.
//
// For a circular field the combine is not merely non-additive but undefined, so a
// bucket with two or more reporting members becomes a gap rather than a bearing
// nobody measured — see buildSeries. handleSeries rejects such a request up
// front so the API answers with an error instead of unexplained gaps; this is
// the library-level guarantee behind that check.
func assembleByGroup(
	buckets []time.Time,
	devices map[string]config.DeviceConfig,
	get func(string) []float64,
	circular bool,
	groupBy, groupFn string,
	groupLabels map[string]string,
) []Series {
	keyOf := GroupKeyFor(groupBy)
	if keyOf == nil {
		return assembleByDevice(buckets, devices, get, circular)
	}
	members := map[string][]string{}
	for id, d := range devices {
		// The combine is a SPATIAL statistic about a room or a floor, so its
		// members are the devices that describe that space (DescribesAmbient),
		// not every device greenhouse charts. An appliance_probe is charted but
		// reports an appliance interior, so averaging it in would move a room's
		// mean toward a freezer's setpoint — see config.ambientClasses. Probes
		// are reached by group_by=device.
		if !d.DescribesAmbient() {
			continue
		}
		place := keyOf(d)
		if place == "" {
			continue
		}
		members[place] = append(members[place], id)
	}

	keys := make([]string, 0, len(members))
	for k := range members {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var out []Series
	for _, k := range keys {
		ids := members[k]
		sort.Strings(ids)
		memberValues := make([][]float64, 0, len(ids))
		for _, id := range ids {
			memberValues = append(memberValues, get(id))
		}
		// A grouped series belongs to no single room, so Room is left empty for
		// floor grouping rather than carrying a floor id in a field named room.
		room := ""
		if groupBy == GroupByRoom {
			room = k
		}
		// The floorplan's name when it publishes one, the id otherwise. Never a
		// label derived from the id — that transform is the client-side guesswork
		// this exists to remove, and moving it into greenhouse would not make it
		// any less of a guess.
		label := k
		if name := groupLabels[k]; name != "" {
			label = name
		}
		out = append(out, buildSeries(k, label, room, buckets, memberValues, circular, groupFn))
	}
	return out
}

// buildSeries combines members into one series over the bucket axis, rounding
// every value and computing the window summary stats. All member slices are
// assumed to be length len(buckets) with NaN for gaps.
//
// Members are combined per groupFn — mean (the default), min or max across the
// members reporting that bucket. Climate is non-additive, so there is no sum. A
// bucket no member reported is a NaN gap: min/max skip non-reporting members
// exactly as the mean does, and a gap never becomes a zero.
//
// circular marks an angular 0–360° field, where that arithmetic is invalid:
// mean(350°, 10°) is 180° (South) though both readings say North. So when
// circular is set:
//
//   - a bucket with exactly ONE reporting member passes that bearing through
//     unchanged — a single instantaneous bearing is always valid;
//   - a bucket with TWO OR MORE reporting members is a GAP, because averaging
//     them would emit a bearing nobody measured. Refusing beats guessing: this
//     package's contract is that greenhouse never emits a confident-but-wrong
//     bearing (see Field.Circular and ValidFnForField);
//   - Min/Max/Mean are NaN (JSON null). They are linear statistics and are
//     undefined on a circular axis for exactly the same reason, whatever the
//     member count — a legend reading "min 10°, max 350°" describes a spread of
//     20° as though it were 340°.
//
// Proper vector averaging (mean of unit vectors) would let the multi-member case
// and the summary both be answered honestly; until it lands, this refuses.
func buildSeries(key, label, place string, buckets []time.Time, members [][]float64, circular bool, groupFn string) Series {
	n := len(buckets)
	s := Series{
		Key:    key,
		Label:  label,
		Room:   place,
		Values: make([]float64, n),
	}
	min, max, sum := math.Inf(1), math.Inf(-1), 0.0
	count := 0
	for i := 0; i < n; i++ {
		var bsum, bmin, bmax float64
		var bcount int
		for _, m := range members {
			if i >= len(m) || math.IsNaN(m[i]) {
				continue // a member that did not report is skipped, for every groupFn
			}
			mv := m[i]
			if bcount == 0 || mv < bmin {
				bmin = mv
			}
			if bcount == 0 || mv > bmax {
				bmax = mv
			}
			bsum += mv
			bcount++
		}
		if bcount == 0 {
			s.Values[i] = math.NaN() // gap: no member reported this bucket
			continue
		}
		if circular && bcount > 1 {
			// No defensible single bearing across members; a gap says "we cannot
			// answer" where an average would say something false with confidence.
			s.Values[i] = math.NaN()
			continue
		}
		var combined float64
		switch groupFn {
		case GroupFnMin:
			combined = bmin
		case GroupFnMax:
			combined = bmax
		default: // mean, and any unrecognised value (callers pre-validate)
			combined = bsum / float64(bcount)
		}
		v := roundTo(combined, valueDP)
		s.Values[i] = v
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
		sum += v
		count++
	}
	if circular || count == 0 {
		s.Min, s.Max, s.Mean = math.NaN(), math.NaN(), math.NaN()
		return s
	}
	s.Min = roundTo(min, valueDP)
	s.Max = roundTo(max, valueDP)
	s.Mean = roundTo(sum/float64(count), valueDP)
	return s
}

// sortedDeviceIDs returns the inventory ids sorted for deterministic output.
func sortedDeviceIDs(devices map[string]config.DeviceConfig) []string {
	ids := make([]string, 0, len(devices))
	for id := range devices {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// environmentalIDs returns the sorted ids of environmental-class devices.
func environmentalIDs(devices map[string]config.DeviceConfig) []string {
	var out []string
	for _, id := range sortedDeviceIDs(devices) {
		if devices[id].ReportsEnvironment() {
			out = append(out, id)
		}
	}
	return out
}

// BuildSeries is the orchestrator: it queries the field series across the
// environmental device set, demuxes the bucketed rows onto the canonical axis,
// and calls AssembleSeries. The query count is independent of device count: the
// builder fans out across the device set via contains(set). It is ONE query,
// except a wall-clock mean, which is two (see foldWallClock).
//
// bucket is the Influx bucket name; win the resolved window; iv the resolved
// interval; field+fn the measurement to chart; groupBy the grouping mode;
// groupFn how a group's members are combined; devices the inventory;
// groupLabels the floorplan display names for grouped series keys; loc the
// timezone. field/fn/groupFn are assumed pre-validated by the caller (FieldFor /
// ValidFnForField / ValidGroupFn).
//
// fn and groupFn are applied in that ORDER and do not commute: fn collapses each
// device's samples within a bucket (inside Influx, completed by the fold for a
// wall-clock interval), then groupFn combines the group's devices here in Go.
func BuildSeries(
	ctx context.Context,
	q influx.Querier,
	bucket string,
	win Window,
	iv Interval,
	field, fn, groupBy, groupFn string,
	devices map[string]config.DeviceConfig,
	groupLabels map[string]string,
	loc *time.Location,
) (SeriesResponse, error) {
	if loc == nil {
		loc = time.UTC
	}
	buckets := BucketStarts(win, iv, loc)
	tz := loc.String()

	ids := environmentalIDs(devices)

	valueByDevice := map[string][]float64{}
	if len(ids) > 0 {
		var err error
		if iv.wallClock() {
			valueByDevice, err = foldWallClock(ctx, q, bucket, ids, field, fn, win, buckets, loc)
		} else {
			valueByDevice, err = querySeries(ctx, q, bucket, ids, field, fn, win, iv.Token, tz, buckets)
		}
		if err != nil {
			return SeriesResponse{}, err
		}
	}

	series := AssembleSeries(buckets, devices, valueByDevice, groupBy, field, groupFn, groupLabels)

	unit := ""
	if f, ok := FieldFor(field); ok {
		unit = f.Unit
	}

	return SeriesResponse{
		Window:   win.Label,
		From:     win.Start.In(loc).Format(time.RFC3339),
		To:       win.Stop.In(loc).Format(time.RFC3339),
		Interval: iv.Token,
		GroupBy:  resolveGroupBy(groupBy),
		Field:    field,
		Unit:     unit,
		Fn:       fn,
		GroupFn:  echoGroupFn(groupBy, groupFn),
		Shape:    ShapeColumns,
		Buckets:  buckets,
		Series:   series,
	}, nil
}

// querySeries runs one field-series query at token and demuxes it onto buckets,
// returning id → per-bucket values (NaN for gaps).
func querySeries(ctx context.Context, q influx.Querier, bucket string, ids []string, field, fn string, win Window, token, tz string, buckets []time.Time) (map[string][]float64, error) {
	flux := influx.BuildFieldSeriesFlux(bucket, ids, field, fn, win.Start, win.Stop, token, tz)
	rows, err := q.Query(ctx, flux)
	if err != nil {
		return nil, err
	}
	out := map[string][]float64{}
	demux(rows, bucketIndex(buckets), out, len(buckets))
	return out, nil
}

// foldWallClock produces per-device values for a WALL-CLOCK (6h) axis (issue
// #36). Influx cannot be asked for those windows: location-aware 6h windows are
// the stretched local shape, and UTC 6h windows sit an hour off 00/06/12/18
// local all summer. So the readings are queried at 1h on the UTC grid — every
// local 6h boundary of a whole-hour zone is a UTC hour boundary — and each hour
// is folded into the display bucket that contains it.
//
// Climate is NOT additive, so how an hour folds depends on fn, and each fold is
// chosen to equal what Influx's own aggregate over the whole 6h window would
// have been (so a 6h chart away from a clock change is unchanged):
//
//   - min / max: the min / max of the hourly mins / maxes.
//   - last: the latest hour that has a reading, which holds the bucket's last
//     reading.
//   - mean: NOT a mean of hourly means. Sensors report on change, so an hour
//     can hold 60 readings or 3, and averaging hourly means would weight the
//     sparse hour like the dense one. The readings are queried as hourly SUM
//     and COUNT instead (a second query) and folded as Σsum / Σcount, which is
//     exactly the sample mean of the 6h window.
//
// A display bucket with no reading in any of its hours stays NaN (a gap). The
// cross-member group_fn runs after this, in AssembleSeries, exactly as for any
// other interval, so fn-then-group_fn order is preserved.
func foldWallClock(ctx context.Context, q influx.Querier, bucket string, ids []string, field, fn string, win Window, buckets []time.Time, loc *time.Location) (map[string][]float64, error) {
	fine, ok := lookupInterval("1h")
	if !ok {
		return nil, fmt.Errorf("climate: 1h interval missing") // unreachable: "1h" is in the allowed set
	}
	fineBuckets := BucketStarts(win, fine, loc)
	into := foldIndex(fineBuckets, buckets)
	tz := loc.String()
	n := len(buckets)

	out := map[string][]float64{}
	switch fn {
	case "mean":
		sums, err := querySeries(ctx, q, bucket, ids, field, influx.FnSum, win, fine.Token, tz, fineBuckets)
		if err != nil {
			return nil, err
		}
		counts, err := querySeries(ctx, q, bucket, ids, field, influx.FnCount, win, fine.Token, tz, fineBuckets)
		if err != nil {
			return nil, err
		}
		for id, sum := range sums {
			out[id] = foldMean(sum, counts[id], into, n)
		}
	case "min", "max", "last":
		vals, err := querySeries(ctx, q, bucket, ids, field, fn, win, fine.Token, tz, fineBuckets)
		if err != nil {
			return nil, err
		}
		for id, v := range vals {
			out[id] = foldExtreme(fn, v, into, n)
		}
	default:
		return nil, fmt.Errorf("climate: no wall-clock fold for fn %q", fn)
	}
	return out, nil
}

// foldIndex maps each fine bucket to the display bucket containing it (the
// greatest display start at or before it), or -1 for one before the first.
// Containment by timestamp rather than a fixed ratio, because a 6h bucket that
// straddles a clock change holds 7 or 5 hours.
func foldIndex(fine, display []time.Time) []int {
	into := make([]int, len(fine))
	j := -1
	for i, f := range fine {
		for j+1 < len(display) && !display[j+1].After(f) {
			j++
		}
		into[i] = j
	}
	return into
}

// foldMean folds hourly sums and counts into Σsum/Σcount per display bucket.
// An hour with no reading (NaN sum, or a count of 0) contributes nothing; a
// bucket none of whose hours has one is NaN.
func foldMean(sum, count []float64, into []int, n int) []float64 {
	s := make([]float64, n)
	c := make([]float64, n)
	for i, j := range into {
		if j < 0 || i >= len(sum) || i >= len(count) {
			continue
		}
		if math.IsNaN(sum[i]) || math.IsNaN(count[i]) || count[i] <= 0 {
			continue
		}
		s[j] += sum[i]
		c[j] += count[i]
	}
	out := make([]float64, n)
	for j := range out {
		if c[j] == 0 {
			out[j] = math.NaN()
			continue
		}
		out[j] = s[j] / c[j]
	}
	return out
}

// foldExtreme folds hourly min/max/last values onto the display buckets.
func foldExtreme(fn string, v []float64, into []int, n int) []float64 {
	out := make([]float64, n)
	for j := range out {
		out[j] = math.NaN()
	}
	for i, j := range into {
		if j < 0 || i >= len(v) || math.IsNaN(v[i]) {
			continue
		}
		switch {
		case math.IsNaN(out[j]), fn == "last":
			out[j] = v[i] // hours arrive in order, so the latest reading wins
		case fn == "min":
			out[j] = math.Min(out[j], v[i])
		case fn == "max":
			out[j] = math.Max(out[j], v[i])
		}
	}
	return out
}

// echoGroupFn reports the member combine to echo in the response: the resolved
// group_fn when the grouping actually combines members, and empty otherwise so
// the field is omitted rather than advertising a combine that never ran.
func echoGroupFn(groupBy, groupFn string) string {
	if !Groups(groupBy) {
		return ""
	}
	if groupFn == "" {
		return DefaultGroupFn
	}
	return groupFn
}

// resolveGroupBy normalises the reported group_by (empty → device).
func resolveGroupBy(groupBy string) string {
	if groupBy == "" {
		return GroupByDevice
	}
	return groupBy
}

// bucketIndex maps each canonical bucket-start (its UnixNano) to its position.
func bucketIndex(buckets []time.Time) map[int64]int {
	m := make(map[int64]int, len(buckets))
	for i, b := range buckets {
		m[b.UnixNano()] = i
	}
	return m
}

// demux folds bucketed rows onto the canonical axis, keyed per device. Each row
// carries a DeviceID, a Time and a Value (with HasValue distinguishing a real
// reading from an empty createEmpty bucket). A device's slice is initialised to
// all-NaN; a row with a real value writes into its resolved bucket. Rows before
// the first bucket or after the last are dropped.
func demux(rows []influx.Row, idx map[int64]int, dst map[string][]float64, n int) {
	if n == 0 {
		return
	}
	starts := make([]int64, 0, len(idx))
	for k := range idx {
		starts = append(starts, k)
	}
	sort.Slice(starts, func(a, b int) bool { return starts[a] < starts[b] })

	ensure := func(id string) []float64 {
		arr := dst[id]
		if arr == nil {
			arr = make([]float64, n)
			for i := range arr {
				arr[i] = math.NaN()
			}
			dst[id] = arr
		}
		return arr
	}

	for _, r := range rows {
		arr := ensure(r.DeviceID)
		if !r.HasValue {
			continue // empty bucket: leave the gap as NaN
		}
		i := resolveBucket(r.Time, idx, starts)
		if i < 0 {
			continue // outside window
		}
		arr[i] = r.Value
	}
}

// resolveBucket maps a row time to a canonical bucket index. It first tries an
// exact left-edge match (the common case when timeSrc:"_start" stamps the
// bucket start). Otherwise it snaps a right-edge / interior stamp back to its
// containing bucket by taking the greatest start strictly less than the stamp;
// a time before the first start returns -1.
func resolveBucket(t time.Time, idx map[int64]int, starts []int64) int {
	key := t.UnixNano()
	if i, ok := idx[key]; ok {
		return i
	}
	lo, hi := 0, len(starts)
	for lo < hi {
		mid := (lo + hi) / 2
		if starts[mid] < key {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo == 0 {
		return -1
	}
	return idx[starts[lo-1]]
}
