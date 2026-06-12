// Package influx is greenhouse's read-side query layer over InfluxDB.
//
// Greenhouse never writes to Influx; it only queries the per-sensor
// environmental telemetry statehouse wrote, turning it into windowed climate
// time-series. The Querier interface is the testable seam (mirroring
// statehouse's local-interface + fake-double pattern on the write side):
// production uses Client, tests use FakeQuerier, and neither the climate logic
// nor the handlers ever touch the influxdb2 SDK directly.
package influx

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Row is a single decoded Flux record. It flattens the tags and columns we
// care about out of a query.FluxRecord. Not every query yields every field:
// callers should treat absent tags as empty strings (the Client decodes them
// defensively).
type Row struct {
	DeviceID string
	Class    string
	Location string
	Field    string
	// Value holds the record's _value when it is numeric (float64); it is the
	// zero value for string-valued and empty (null) records.
	Value float64
	// HasValue distinguishes a real numeric reading from an empty
	// (createEmpty:true) bucket, where the SDK yields a nil _value. Without it
	// a genuine 0.0 reading (e.g. 0°C) would be indistinguishable from a gap.
	HasValue bool
	// Text holds the record's _value when it is a string. Climate readings are
	// numeric gauges, so Text is normally empty; it is preserved for symmetry
	// with the decoded record.
	Text string
	Time time.Time
}

// Querier is the minimal subset of Influx querying that greenhouse needs.
// Defining it locally keeps the climate logic decoupled from the SDK and lets
// tests substitute FakeQuerier without standing up a database.
type Querier interface {
	// Query runs a Flux script and returns the decoded rows. An error is
	// returned for transport/auth failures or a malformed result; an empty
	// (but non-nil-error) result simply yields a nil/empty slice.
	Query(ctx context.Context, flux string) ([]Row, error)
	// Ping reports whether the backend is reachable.
	Ping(ctx context.Context) bool
}

// fluxTime renders t for use inside range(start:, stop:). Influx accepts
// RFC3339; we force UTC so the bounds are unambiguous regardless of the
// caller's location.
func fluxTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// deviceSet renders a Flux array literal of quoted device ids, e.g.
// `["a", "b"]`. It is used inside contains(value: r.device_id, set: [...]) so a
// single query can fan out across a whole set of devices (keeping the query
// count device-count-independent).
func deviceSet(deviceIDs []string) string {
	quoted := make([]string, len(deviceIDs))
	for i, id := range deviceIDs {
		quoted[i] = fmt.Sprintf("%q", id)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}
