package climate

import (
	"context"
	"encoding/json"
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

// fixedAxisStart returns the first canonical bucket start for a FIXED (sub-day)
// interval: start snapped DOWN onto the interval grid anchored at the local
// midnight of start's own date in loc. start is expected to already be in loc.
//
// Snapping is what keeps the Go-owned axis on the same boundaries as Influx.
// influx.BuildFieldSeriesFlux calls aggregateWindow with
// location: timezone.location(tz), which anchors its windows to local midnight
// in that zone, so its timeSrc:"_start" stamps land on the grid regardless of
// where the query range begins. An UNSNAPPED axis stepping straight off a
// caller-derived start therefore had boundaries Influx could never exact-match,
// and resolveBucket's snap-back rule — correct for a right-edge or interior
// stamp — could not tell those apart from an Influx LEFT edge falling inside a
// Go bucket. Every row landed in the preceding bucket and the whole series was
// reported one bucket early. That hit every "<N>h" rolling window (their start
// is now - N hours, deliberately not midnight-aligned) and any custom `from` off
// the grid, at every sub-day interval. See issue #18.
//
// Anchoring at LOCAL MIDNIGHT rather than the Unix epoch is the load-bearing
// part: a 6h grid must be 00/06/12/18 local in both GMT and BST, which an
// epoch-anchored (UTC) grid gets an hour wrong for the whole of British Summer
// Time.
//
// Accepted trade: for an off-grid window the first bucket widens back to its
// grid boundary, so it is labelled a little before win.Start. That slice carries
// no in-window data — the query range still starts at win.Start — and the
// alternative is the shifted axis above. Countinghouse made the same trade.
//
// Known caveat, pre-existing and unchanged by the snap: a sub-day interval over
// a window that CROSSES a DST transition still steps by fixed duration from this
// anchor, so after the changeover it drifts an hour off Flux's stretched local
// grid. It needs both a sub-day interval and a window spanning the transition;
// the calendar (1d) branch is unaffected.
func fixedAxisStart(start time.Time, iv Interval, loc *time.Location) time.Time {
	anchor := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, loc)
	off := start.Sub(anchor) % iv.Duration
	if off < 0 {
		// Defensive: a zone whose local midnight does not exist could put start
		// before its own anchor. Go's % keeps the dividend's sign, so normalise
		// to [0, Duration) rather than snapping FORWARD past the window start.
		off += iv.Duration
	}
	return start.Add(-off)
}

// BucketStarts returns the canonical local-timezone bucket-start axis for win
// at iv: the ascending list of bucket starts covering win.Start up to (but not
// including) win.Stop. This axis is the single source of truth every series
// aligns to; Influx results are demuxed onto it and gaps are NaN-filled.
//
// For calendar intervals (1d) the axis steps by CALENDAR day in loc using
// time.Date(year, month, day+1, ...), so a London day that is 23h (spring
// forward) or 25h (autumn back) is still a single bucket starting at local
// midnight — DST-correct. For fixed (sub-day) intervals the axis steps by
// iv.Duration from fixedAxisStart, i.e. from the grid cell CONTAINING the
// window start rather than from the raw start — see fixedAxisStart for why.
//
// countBuckets in interval.go computes this axis's LENGTH without materializing
// it, to guard MaxBuckets cheaply. The two must agree exactly for any in-cap
// window; both derive the first fixed bucket from fixedAxisStart so they cannot
// drift apart.
func BucketStarts(win Window, iv Interval, loc *time.Location) []time.Time {
	if loc == nil {
		loc = time.UTC
	}
	start := win.Start.In(loc)
	stop := win.Stop

	var out []time.Time
	if iv.Calendar {
		// Step by calendar day, anchored at the local midnight of start's date.
		cur := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, loc)
		for cur.Before(stop) {
			out = append(out, cur)
			cur = time.Date(cur.Year(), cur.Month(), cur.Day()+1, 0, 0, 0, 0, loc)
		}
		return out
	}

	for cur := fixedAxisStart(start, iv, loc); cur.Before(stop); cur = cur.Add(iv.Duration) {
		out = append(out, cur)
	}
	return out
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
		return assembleByGroup(buckets, devices, get, circular, groupBy, groupFn)
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
		if !d.ReportsEnvironment() {
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
) []Series {
	keyOf := GroupKeyFor(groupBy)
	if keyOf == nil {
		return assembleByDevice(buckets, devices, get, circular)
	}
	members := map[string][]string{}
	for id, d := range devices {
		if !d.ReportsEnvironment() {
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
		out = append(out, buildSeries(k, k, room, buckets, memberValues, circular, groupFn))
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

// BuildSeries is the orchestrator: it runs ONE Influx query (the field series
// across the environmental device set), demuxes the bucketed rows onto the
// canonical axis, and calls AssembleSeries. The query count is independent of
// device count: the builder fans out across the device set via contains(set).
//
// bucket is the Influx bucket name; win the resolved window; iv the resolved
// interval; field+fn the measurement to chart; groupBy the grouping mode;
// groupFn how a group's members are combined; devices the inventory; loc the
// timezone. field/fn/groupFn are assumed pre-validated by the caller (FieldFor /
// ValidFnForField / ValidGroupFn).
//
// fn and groupFn are applied in that ORDER and do not commute: fn collapses each
// device's samples within a bucket inside Influx, then groupFn combines the
// group's devices here in Go.
func BuildSeries(
	ctx context.Context,
	q influx.Querier,
	bucket string,
	win Window,
	iv Interval,
	field, fn, groupBy, groupFn string,
	devices map[string]config.DeviceConfig,
	loc *time.Location,
) (SeriesResponse, error) {
	if loc == nil {
		loc = time.UTC
	}
	buckets := BucketStarts(win, iv, loc)
	idx := bucketIndex(buckets)
	tz := loc.String()

	ids := environmentalIDs(devices)

	valueByDevice := map[string][]float64{}
	if len(ids) > 0 {
		flux := influx.BuildFieldSeriesFlux(bucket, ids, field, fn, win.Start, win.Stop, iv.Token, tz)
		rows, err := q.Query(ctx, flux)
		if err != nil {
			return SeriesResponse{}, err
		}
		demux(rows, idx, valueByDevice, len(buckets))
	}

	series := AssembleSeries(buckets, devices, valueByDevice, groupBy, field, groupFn)

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

// resolveGroupBy normalises the reported group_by (empty → device).
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
