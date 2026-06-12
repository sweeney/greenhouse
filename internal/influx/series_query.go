package influx

import (
	"fmt"
	"time"
)

// BuildFieldSeriesFlux builds the per-bucket climate series for ONE field over a
// SET of devices, aggregated with fn (mean/min/max/last) on DST-aware local
// buckets. It is the heart of greenhouse: a plain gauge query over the
// device_environment measurement with no counter/integral/reset handling
// (climate fields are not cumulative).
//
//   - filter selects the device_environment measurement and r._field == field.
//   - the device set fans out across devices via contains(set: [...]), keeping
//     the query count independent of device count.
//   - aggregateWindow(every: interval, fn: <fn>, timeSrc: "_start",
//     location: timezone.location(tz), createEmpty: true) collapses each bucket
//     to its aggregate on DST-aware local boundaries, emitting empty buckets so
//     the axis is dense.
//
// timeSrc:"_start" is REQUIRED: Influx's aggregateWindow stamps the right edge
// (_stop) by default, which shifts every value one bucket late relative to the
// Go-owned canonical left-edge axis. The Go layer demuxes rows onto that axis
// and zero/NaN-fills gaps. No pad is needed — a bucket aggregate is
// self-contained (unlike countinghouse's counter difference()).
func BuildFieldSeriesFlux(bucket string, deviceIDs []string, field, fn string, start, stop time.Time, interval, tz string) string {
	return fmt.Sprintf(`import "timezone"

from(bucket: %q)
  |> range(start: %s, stop: %s)
  |> filter(fn: (r) => r._measurement == "device_environment" and r._field == %q)
  |> filter(fn: (r) => contains(value: r.device_id, set: %s))
  |> aggregateWindow(every: %s, fn: %s, timeSrc: "_start", location: timezone.location(name: %q), createEmpty: true)`,
		bucket,
		fluxTime(start),
		fluxTime(stop),
		field,
		deviceSet(deviceIDs),
		interval,
		fn,
		tz,
	)
}

// BuildLatestFlux builds the "most recent reading across all fields" query for a
// single device, used by GET /devices/{id}/latest. last() per field collapses
// each field's series to its newest point; group(columns:["_field"]) keeps the
// fields separate so the row carries each field's _field/_value/_time. The range
// reaches back far enough (Unix epoch) that a device reporting infrequently
// still yields its latest value.
func BuildLatestFlux(bucket, deviceID string) string {
	return fmt.Sprintf(`from(bucket: %q)
  |> range(start: 1970-01-01T00:00:00Z)
  |> filter(fn: (r) => r._measurement == "device_environment")
  |> filter(fn: (r) => r.device_id == %q)
  |> group(columns: ["_field"])
  |> last()`,
		bucket, deviceID)
}
