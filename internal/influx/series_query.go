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
//
// NOT filtered by site, and that is deliberate rather than forgotten — see #13.
// statehouse tags every point with its site, but only since 2026-08-10, so a bare
// `r.site == <id>` would exclude every earlier point and silently empty out the
// history. Correct scoping needs the operator to declare which site owns the untagged
// history, which is a config change, not a change to this query. Harmless while one
// site writes to the bucket; must land before a second one does.
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

// DefaultLatestLookback bounds how far GET /devices/{id}/latest looks back for a
// device's most recent reading. It must comfortably exceed the slowest real
// reporter's interval so an infrequent device still returns; 7 days does, while
// keeping the scan cost recency-proportional rather than retention-proportional.
// A device silent for longer than this is itself a staleness signal.
const DefaultLatestLookback = "7d"

// BuildLatestFlux builds the "most recent reading across all fields" query for a
// single device, used by GET /devices/{id}/latest. last() per field collapses
// each field's series to its newest point; group(columns:["_field"]) keeps the
// fields separate so the row carries each field's _field/_value/_time.
//
// lookback bounds the range (e.g. "7d") so Influx can prune by time: the cost
// scales with recency, not with the bucket's ~2-year retention. Ranging from
// the Unix epoch (the previous behaviour) forced a whole-history scan per call
// on an endpoint explicitly meant to be polled by dashboards.
func BuildLatestFlux(bucket, deviceID, lookback string) string {
	return fmt.Sprintf(`from(bucket: %q)
  |> range(start: -%s)
  |> filter(fn: (r) => r._measurement == "device_environment")
  |> filter(fn: (r) => r.device_id == %q)
  |> group(columns: ["_field"])
  |> last()`,
		bucket, lookback, deviceID)
}
