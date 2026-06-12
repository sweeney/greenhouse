package climate

import "sort"

// Field is one environmental measurement greenhouse can chart. Name is the
// Influx _field key (e.g. "temperature_c"); Unit is the display unit; DefaultFn
// is the aggregation applied per bucket when the caller does not specify one.
//
// Climate fields are plain gauge readings, so the default aggregation is mean
// (never sum — climate is NON-additive). min/max/last are also offered for
// "coldest in the bucket", "peak gust", "latest reading", etc.
type Field struct {
	Name      string `json:"name"`
	Unit      string `json:"unit"`
	DefaultFn string `json:"default_fn"`
}

// DefaultField is the field used when a series request omits ?field=.
const DefaultField = "temperature_c"

// fields is the registry of chartable environmental fields, keyed by Influx
// _field name. Units mirror statehouse's device_environment measurement.
var fields = map[string]Field{
	"temperature_c":   {Name: "temperature_c", Unit: "°C", DefaultFn: "mean"},
	"humidity_pct":    {Name: "humidity_pct", Unit: "%", DefaultFn: "mean"},
	"pressure_hpa":    {Name: "pressure_hpa", Unit: "hPa", DefaultFn: "mean"},
	"wind_speed_ms":   {Name: "wind_speed_ms", Unit: "m/s", DefaultFn: "mean"},
	"wind_dir_deg":    {Name: "wind_dir_deg", Unit: "°", DefaultFn: "mean"},
	"rainfall_mm":     {Name: "rainfall_mm", Unit: "mm", DefaultFn: "mean"},
	"illuminance_lux": {Name: "illuminance_lux", Unit: "lux", DefaultFn: "mean"},
	"uv_index":        {Name: "uv_index", Unit: "index", DefaultFn: "mean"},
}

// allowedFns is the set of aggregation functions a series request may use. mean
// is the default; sum is deliberately absent (climate is non-additive).
var allowedFns = map[string]struct{}{
	"mean": {},
	"min":  {},
	"max":  {},
	"last": {},
}

// DefaultFn is the aggregation applied when the caller omits ?fn=.
const DefaultFn = "mean"

// FieldFor returns the Field for name and whether it is a known chartable field.
func FieldFor(name string) (Field, bool) {
	f, ok := fields[name]
	return f, ok
}

// ValidFn reports whether fn is an allowed aggregation function. The empty
// string is NOT valid here; callers default to DefaultFn before validating.
func ValidFn(fn string) bool {
	_, ok := allowedFns[fn]
	return ok
}

// AllowedFns returns the allowed aggregation functions, sorted for stable
// output (error messages, OpenAPI enums).
func AllowedFns() []string {
	out := make([]string, 0, len(allowedFns))
	for fn := range allowedFns {
		out = append(out, fn)
	}
	sort.Strings(out)
	return out
}

// Fields returns the field registry as a slice sorted by name, for the /fields
// endpoint and as the catalog fallback hint.
func Fields() []Field {
	names := make([]string, 0, len(fields))
	for n := range fields {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]Field, 0, len(names))
	for _, n := range names {
		out = append(out, fields[n])
	}
	return out
}

// FieldNames returns the registry field names, sorted. Used as the /devices
// `fields` hint fallback when the device config carries no explicit list.
func FieldNames() []string {
	names := make([]string, 0, len(fields))
	for n := range fields {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
