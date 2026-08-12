package climate

import (
	"encoding/json"
	"math"
	"time"
)

// Series payload shapes, selectable by the `shape` query param on the series
// endpoints.
//
//   - ShapeColumns (default): the columnar SeriesResponse — a shared Buckets axis
//     plus per-series value arrays. Ideal for web charting libraries, where each
//     array drops straight into a dataset.
//   - ShapeRows: the row-oriented RowsResponse — a flat list of one row per
//     (series, bucket). Idiomatic for Codable consumers (decode []SeriesPoint)
//     and grouped native charts, e.g. Swift Charts' foregroundStyle(by: key).
const (
	ShapeColumns = "columns"
	ShapeRows    = "rows"
)

// ValidShape reports whether s is an accepted shape ("" defaults to columns).
func ValidShape(s string) bool {
	return s == "" || s == ShapeColumns || s == ShapeRows
}

// SeriesPoint is one (series, bucket) sample in the row-oriented form. Value is
// already rounded by AssembleSeries; a gap (empty bucket) is NaN and marshals
// to JSON null (see MarshalJSON).
type SeriesPoint struct {
	Key   string    `json:"key"`   // series key (device id or room id)
	Time  time.Time `json:"time"`  // bucket start, RFC3339 with the configured tz offset
	Value float64   `json:"value"` // the field value for this bucket (null when no reading)
}

// seriesPointJSON is the wire form with Value as *float64 so NaN gaps become
// JSON null.
type seriesPointJSON struct {
	Key   string    `json:"key"`
	Time  time.Time `json:"time"`
	Value *float64  `json:"value"`
}

// MarshalJSON renders a NaN Value as JSON null.
func (p SeriesPoint) MarshalJSON() ([]byte, error) {
	out := seriesPointJSON{Key: p.Key, Time: p.Time, Value: nullable(p.Value)}
	return json.Marshal(out)
}

// SeriesMeta is per-series metadata (no per-bucket arrays) carried alongside the
// rows so consumers still have labels and summary stats for legends without
// scanning the row list. Min/Max/Mean are NaN for an empty series (→ null).
type SeriesMeta struct {
	Key   string  `json:"key"`
	Label string  `json:"label"`
	Room  string  `json:"room,omitempty"`
	Min   float64 `json:"min"`
	Max   float64 `json:"max"`
	Mean  float64 `json:"mean"`
}

// seriesMetaJSON renders the summary stats as nullable on the wire.
type seriesMetaJSON struct {
	Key   string   `json:"key"`
	Label string   `json:"label"`
	Room  string   `json:"room,omitempty"`
	Min   *float64 `json:"min"`
	Max   *float64 `json:"max"`
	Mean  *float64 `json:"mean"`
}

// MarshalJSON renders NaN summary stats as JSON null.
func (m SeriesMeta) MarshalJSON() ([]byte, error) {
	out := seriesMetaJSON{
		Key:   m.Key,
		Label: m.Label,
		Room:  m.Room,
		Min:   nullable(m.Min),
		Max:   nullable(m.Max),
		Mean:  nullable(m.Mean),
	}
	return json.Marshal(out)
}

// RowsResponse is the row-oriented ("tidy"/long) form of a SeriesResponse: one
// flat row per (series, bucket), plus lightweight per-series metadata. Rows are
// ordered by series (in the columnar response's series order) then by bucket
// time, so each series' points are contiguous and already time-sorted. It
// carries the same field/unit/fn metadata as the columnar form.
type RowsResponse struct {
	Window   string        `json:"window"`
	From     string        `json:"from"`
	To       string        `json:"to"`
	Interval string        `json:"interval"`
	GroupBy  string        `json:"group_by"`
	Field    string        `json:"field"`
	Unit     string        `json:"unit"`
	Fn       string        `json:"fn"`
	Shape    string        `json:"shape"` // "rows"
	Series   []SeriesMeta  `json:"series"`
	Rows     []SeriesPoint `json:"rows"`
}

// Rows converts the columnar SeriesResponse into the row-oriented RowsResponse.
// It is a pure reshape: values are copied as-is (already rounded, NaN gaps
// preserved), and every series contributes exactly len(Buckets) rows.
func (r SeriesResponse) Rows() RowsResponse {
	out := RowsResponse{
		Window:   r.Window,
		From:     r.From,
		To:       r.To,
		Interval: r.Interval,
		GroupBy:  r.GroupBy,
		Field:    r.Field,
		Unit:     r.Unit,
		Fn:       r.Fn,
		Shape:    ShapeRows,
		Series:   make([]SeriesMeta, 0, len(r.Series)),
		Rows:     make([]SeriesPoint, 0, len(r.Series)*len(r.Buckets)),
	}
	for _, s := range r.Series {
		out.Series = append(out.Series, SeriesMeta{
			Key:   s.Key,
			Label: s.Label,
			Room:  s.Room,
			Min:   s.Min,
			Max:   s.Max,
			Mean:  s.Mean,
		})
		for i, t := range r.Buckets {
			out.Rows = append(out.Rows, SeriesPoint{
				Key:   s.Key,
				Time:  t,
				Value: at(s.Values, i),
			})
		}
	}
	return out
}

// at returns a[i] or NaN if i is out of range (defensive: arrays should already
// be len(Buckets) after AssembleSeries).
func at(a []float64, i int) float64 {
	if i >= 0 && i < len(a) {
		return a[i]
	}
	return math.NaN()
}
