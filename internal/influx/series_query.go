package influx

import (
	"fmt"
	"time"
)

// intervalDurations mirrors climate's allowed interval tokens, so the builder
// can tell a calendar day from a sub-day interval without importing climate.
var intervalDurations = map[string]time.Duration{
	"5m":  5 * time.Minute,
	"15m": 15 * time.Minute,
	"30m": 30 * time.Minute,
	"1h":  time.Hour,
	"6h":  6 * time.Hour,
	"1d":  24 * time.Hour,
}

// windowLocation renders the aggregateWindow location: argument for a series
// query at `interval` in the house timezone `tz` (issue #36).
//
// Only a CALENDAR interval (1d) is windowed on local time. Every sub-day
// interval is windowed on the UTC grid, and that is deliberate:
//
//   - Influx's location-aware aggregateWindow lays sub-day windows on LOCAL
//     WALL CLOCK. On the autumn clock change (Europe/London, 02:00 BST →
//     01:00 GMT) the window that opens just before the change runs to the next
//     wall time that only happens once, 02:00 GMT. At 15m the 01:45 BST window
//     was 75 minutes long, nothing was stamped inside the repeated hour, and
//     /series reported every probe as null for 01:00Z–01:45Z. Observed in prod
//     on 2026-10-25; countinghouse hit the same thing (#49).
//   - The UTC grid has no repeated or missing hour to mishandle: every window
//     is exactly `every` long. For a zone whose offset is always a whole number
//     of hours (config refuses any other; see config.checkWholeHourZone) the
//     UTC grid at 1h and finer IS the local grid, so bucket labels do not move —
//     only the clock-change hours stop being stretched.
//   - A calendar day is the one place stretching is right: the London day is
//     23h or 25h across a change and should still be ONE bucket from local
//     midnight to local midnight. Flux gets that right, so 1d keeps location:.
//
// 6h is never queried at its own width: climate queries it at 1h and folds onto
// a local wall-clock axis in Go, because a 6h UTC grid is not the 00/06/12/18
// local grid in BST. A 6h token passed here is windowed on UTC like any
// sub-day interval — correct as UTC windows, and never what /series asks for.
//
// The UTC case is spelled out (timezone.utc) rather than left to Flux's default
// so a script-level `option location` can never change it. UTC is also the
// DEFAULT: only an interval known to be a day or longer gets the named zone, so
// a token added to climate's intervals and not mirrored above fails safe.
func windowLocation(interval, tz string) string {
	if d, ok := intervalDurations[interval]; ok && d >= 24*time.Hour {
		return fmt.Sprintf("timezone.location(name: %q)", tz)
	}
	return "timezone.utc"
}

// BuildFieldSeriesFlux builds the per-bucket climate series for ONE field over a
// SET of devices, aggregated with fn (mean/min/max/last). It is the heart of greenhouse: a plain gauge query over the
// device_environment measurement with no counter/integral/reset handling
// (climate fields are not cumulative).
//
//   - filter selects the device_environment measurement and r._field == field.
//   - the device set fans out across devices via contains(set: [...]), keeping
//     the query count independent of device count.
//   - aggregateWindow(every: interval, fn: <fn>, timeSrc: "_start",
//     location: <windowLocation>, createEmpty: true) collapses each bucket to
//     its aggregate — on the UTC grid for a sub-day interval, on local calendar
//     days for 1d (issue #36) — emitting empty buckets so the axis is dense.
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
  |> aggregateWindow(every: %s, fn: %s, timeSrc: "_start", location: %s, createEmpty: true)`,
		bucket,
		fluxTime(start),
		fluxTime(stop),
		field,
		deviceSet(deviceIDs),
		interval,
		fn,
		windowLocation(interval, tz),
	)
}

// BuildFieldSumCountFlux builds, per window, the SUM and the COUNT of one
// field's readings over a set of devices, in ONE pass over the data (issue #36,
// PR #37 review). climate folds a 6h mean as Σsum/Σcount over its hours.
//
// The two numbers must describe the same readings. Fetched as two queries, a
// reading statehouse writes between them (rolling windows end at "now") is
// counted in one and missing from the other, and the open bucket — the one a
// dashboard is watching — reads low: 10 °C when every reading was 20 °C. One
// reduce() produces both from one read, so they cannot disagree.
//
//   - window(every:, location:) lays the same grid aggregateWindow would (see
//     windowLocation; the fold only ever asks for 1h, on UTC). Window bounds
//     are truncated to the range, so an off-grid start stamps the first row at
//     the range start, as with aggregateWindow.
//   - reduce() accumulates sum and count as FLOATS (0.0 / 1.0): a Flux integer
//     decodes as no value in the client, which would turn every mean into a gap.
//     float(v:) guards against a field ever written as an integer.
//   - duplicate(_start → _time) stamps each row at its window's LEFT edge (the
//     timeSrc:"_start" convention), and window(every: inf) undoes the
//     windowing so each device's rows come back as one table.
//
// A window with no reading yields no row (window() does not create empty
// windows by default); the fold treats an absent hour as contributing nothing.
func BuildFieldSumCountFlux(bucket string, deviceIDs []string, field string, start, stop time.Time, interval, tz string) string {
	return fmt.Sprintf(`import "timezone"

from(bucket: %q)
  |> range(start: %s, stop: %s)
  |> filter(fn: (r) => r._measurement == "device_environment" and r._field == %q)
  |> filter(fn: (r) => contains(value: r.device_id, set: %s))
  |> window(every: %s, location: %s)
  |> reduce(identity: {sum: 0.0, count: 0.0}, fn: (r, accumulator) => ({sum: accumulator.sum + float(v: r._value), count: accumulator.count + 1.0}))
  |> duplicate(column: "_start", as: "_time")
  |> window(every: inf)`,
		bucket,
		fluxTime(start),
		fluxTime(stop),
		field,
		deviceSet(deviceIDs),
		interval,
		windowLocation(interval, tz),
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
