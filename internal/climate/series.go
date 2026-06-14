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
	GroupByDevice   = "device"
	GroupByLocation = "location"
)

// environmentalClass is the device class greenhouse charts.
const environmentalClass = "environmental_sensor"

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
	Key      string `json:"key"`
	Label    string `json:"label"`
	Location string `json:"location,omitempty"`

	Values []float64 `json:"values"`

	Min  float64 `json:"min"`
	Max  float64 `json:"max"`
	Mean float64 `json:"mean"`
}

// seriesJSON is the wire form of Series with Values as []*float64 so NaN gaps
// marshal to null and the summary stats drop to null when the series is empty.
type seriesJSON struct {
	Key      string     `json:"key"`
	Label    string     `json:"label"`
	Location string     `json:"location,omitempty"`
	Values   []*float64 `json:"values"`
	Min      *float64   `json:"min"`
	Max      *float64   `json:"max"`
	Mean     *float64   `json:"mean"`
}

// MarshalJSON renders NaN values (empty buckets / empty-series stats) as JSON
// null, keeping the Go-side type a plain []float64 (per the package contract)
// while emitting valid JSON.
func (s Series) MarshalJSON() ([]byte, error) {
	out := seriesJSON{
		Key:      s.Key,
		Label:    s.Label,
		Location: s.Location,
		Values:   make([]*float64, len(s.Values)),
		Min:      nullable(s.Min),
		Max:      nullable(s.Max),
		Mean:     nullable(s.Mean),
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
	Window   string      `json:"window"`
	From     string      `json:"from"`
	To       string      `json:"to"`
	Interval string      `json:"interval"`
	GroupBy  string      `json:"group_by"`
	Field    string      `json:"field"`
	Unit     string      `json:"unit"`
	Fn       string      `json:"fn"`
	Shape    string      `json:"shape"` // "columns"
	Buckets  []time.Time `json:"buckets"`
	Series   []Series    `json:"series"`
}

// BucketStarts returns the canonical local-timezone bucket-start axis for win
// at iv: the ascending list of bucket starts from win.Start up to (but not
// including) win.Stop. This axis is the single source of truth every series
// aligns to; Influx results are demuxed onto it and gaps are NaN-filled.
//
// For calendar intervals (1d) the axis steps by CALENDAR day in loc using
// time.Date(year, month, day+1, ...), so a London day that is 23h (spring
// forward) or 25h (autumn back) is still a single bucket starting at local
// midnight — DST-correct. For fixed (sub-day) intervals the axis steps by
// iv.Duration from the (local) window start.
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

	for cur := start; cur.Before(stop); cur = cur.Add(iv.Duration) {
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
//   - location: members sharing a Location are combined with the MEAN of their
//     per-bucket readings (NON-additive — never a sum). A bucket where no member
//     reported stays NaN.
func AssembleSeries(
	buckets []time.Time,
	devices map[string]config.DeviceConfig,
	valueByDevice map[string][]float64,
	groupBy string,
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

	switch groupBy {
	case GroupByLocation:
		return assembleByLocation(buckets, devices, get)
	default: // device and unknown fall back to per-device
		return assembleByDevice(buckets, devices, get)
	}
}

// assembleByDevice yields one series per environmental device, sorted by id.
func assembleByDevice(
	buckets []time.Time,
	devices map[string]config.DeviceConfig,
	get func(string) []float64,
) []Series {
	var out []Series
	for _, id := range sortedDeviceIDs(devices) {
		d := devices[id]
		if d.Class != environmentalClass {
			continue
		}
		label := d.DisplayName
		if label == "" {
			label = id
		}
		out = append(out, buildSeries(id, label, d.Location, buckets, [][]float64{get(id)}))
	}
	return out
}

// assembleByLocation yields one series per distinct non-empty Location over
// environmental devices, combining member readings with the per-bucket MEAN.
func assembleByLocation(
	buckets []time.Time,
	devices map[string]config.DeviceConfig,
	get func(string) []float64,
) []Series {
	members := map[string][]string{}
	for id, d := range devices {
		if d.Class != environmentalClass {
			continue
		}
		if d.Location == "" {
			continue
		}
		members[d.Location] = append(members[d.Location], id)
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
		s := buildSeries(k, k, k, buckets, memberValues)
		out = append(out, s)
	}
	return out
}

// buildSeries combines member per-bucket slices with the MEAN (ignoring NaN
// gaps), rounds every value, and computes the window summary stats. All member
// slices are assumed to be length len(buckets) with NaN for gaps.
func buildSeries(key, label, location string, buckets []time.Time, members [][]float64) Series {
	n := len(buckets)
	s := Series{
		Key:      key,
		Label:    label,
		Location: location,
		Values:   make([]float64, n),
	}
	min, max, sum := math.Inf(1), math.Inf(-1), 0.0
	count := 0
	for i := 0; i < n; i++ {
		var bsum float64
		var bcount int
		for _, m := range members {
			if i < len(m) && !math.IsNaN(m[i]) {
				bsum += m[i]
				bcount++
			}
		}
		if bcount == 0 {
			s.Values[i] = math.NaN() // gap: no member reported this bucket
			continue
		}
		v := roundTo(bsum/float64(bcount), valueDP)
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
	if count == 0 {
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
		if devices[id].Class == environmentalClass {
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
// devices the inventory; loc the timezone. field/fn are assumed pre-validated
// by the caller (FieldFor / ValidFn).
func BuildSeries(
	ctx context.Context,
	q influx.Querier,
	bucket string,
	win Window,
	iv Interval,
	field, fn, groupBy string,
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

	series := AssembleSeries(buckets, devices, valueByDevice, groupBy)

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
		Shape:    ShapeColumns,
		Buckets:  buckets,
		Series:   series,
	}, nil
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
