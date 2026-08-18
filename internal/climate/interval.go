package climate

import (
	"fmt"
	"sort"
	"time"
)

// MaxBuckets caps how many buckets a single series response may carry. A finer
// interval over a long window can blow the response size up unboundedly; rather
// than silently degrade, ResolveInterval errors and asks the caller for a
// coarser interval.
//
// The value is sized to the finest resolution we intend to support over a
// week-scale window: 5-minute buckets over 7 days is 7*24*12 = 2016 buckets,
// and a rolling week straddling the autumn DST change (a 25h day) can reach
// ~2028. 2500 is a round number comfortably above that, while still rejecting
// genuinely excessive resolution*span (e.g. a month at 5m is ~8640 buckets,
// rejected with a coarser suggestion). The bound is universal: because any
// finer interval over a longer window is rejected down to this cap, it is also
// the worst-case bucket count — and at ~10 climate series a 2016-bucket
// columnar payload is only a few hundred KB.
const MaxBuckets = 2500

// Interval is one allowed bucketing granularity.
//
// Token is the Flux duration literal passed to aggregateWindow(every:) (and to
// the influx series builder). Duration is the Go-side fixed length used to step
// the canonical axis. Calendar marks day-and-larger intervals whose real length
// is NOT a fixed Duration: a London calendar day is 23h or 25h across a DST
// changeover, so the axis must step by calendar date (time.Date day+1) rather
// than by adding Duration.
type Interval struct {
	Token    string
	Duration time.Duration
	Calendar bool
}

// intervals is the allowed set, smallest first. Order matters: DefaultInterval
// and the coarser-fallback logic walk it ascending.
var intervals = []Interval{
	{Token: "5m", Duration: 5 * time.Minute},
	{Token: "15m", Duration: 15 * time.Minute},
	{Token: "30m", Duration: 30 * time.Minute},
	{Token: "1h", Duration: time.Hour},
	{Token: "6h", Duration: 6 * time.Hour},
	{Token: "1d", Duration: 24 * time.Hour, Calendar: true},
}

// intervalByToken indexes intervals by their Flux token.
var intervalByToken = func() map[string]Interval {
	m := make(map[string]Interval, len(intervals))
	for _, iv := range intervals {
		m[iv.Token] = iv
	}
	return m
}()

// AllowedIntervals returns the allowed Flux tokens, smallest first. Useful for
// error messages and OpenAPI enums.
func AllowedIntervals() []string {
	out := make([]string, len(intervals))
	for i, iv := range intervals {
		out[i] = iv.Token
	}
	return out
}

// lookupInterval returns the Interval for a Flux token, or false if not allowed.
func lookupInterval(token string) (Interval, bool) {
	iv, ok := intervalByToken[token]
	return iv, ok
}

// DefaultInterval picks a sensible bucket size for a window when the caller
// supplies none:
//
//   - today → 1h (24-ish buckets)
//   - week  → 1d (7 buckets)
//   - month → 1d (~30 buckets)
//   - custom + rolling (<N>d/<N>h) → chosen by elapsed span so the bucket count
//     stays modest (≲ ~300): ≤2d → 1h, ≤14d → 6h, otherwise 1d. So 7d→6h,
//     30d→1d, 24h→1h.
func DefaultInterval(win Window) string {
	switch win.Label {
	case WindowToday:
		return "1h"
	case WindowWeek, WindowMonth:
		return "1d"
	}
	// custom + rolling windows: pick by elapsed span.
	span := win.Stop.Sub(win.Start)
	switch {
	case span <= 2*24*time.Hour:
		return "1h"
	case span <= 14*24*time.Hour:
		return "6h"
	default:
		return "1d"
	}
}

// ResolveInterval resolves the effective bucketing for a window. When requested
// is empty the smart default for the window is used; otherwise requested must
// be one of the allowed tokens. The resulting bucket count over the window is
// checked against MaxBuckets and rejected (with a message naming the cap and
// suggesting a coarser interval) if exceeded.
//
// loc is the timezone the axis is computed in (so calendar-day counts are
// DST-correct).
func ResolveInterval(win Window, requested string, loc *time.Location) (Interval, error) {
	if loc == nil {
		return Interval{}, fmt.Errorf("climate: nil location")
	}

	token := requested
	if token == "" {
		token = DefaultInterval(win)
	}

	iv, ok := lookupInterval(token)
	if !ok {
		return Interval{}, fmt.Errorf("climate: interval %q not allowed; choose one of %v", requested, AllowedIntervals())
	}

	n := countBuckets(win, iv, loc)
	if n > MaxBuckets {
		coarser := suggestCoarser(win, loc)
		return Interval{}, fmt.Errorf("climate: interval %q yields %d buckets over the window, exceeding the cap of %d; request a coarser interval (e.g. %q)", token, n, MaxBuckets, coarser)
	}

	return iv, nil
}

// suggestCoarser returns the smallest allowed interval whose bucket count over
// win is within MaxBuckets, defaulting to the coarsest ("1d") if even that is
// over (it never is for realistic windows).
func suggestCoarser(win Window, loc *time.Location) string {
	for _, iv := range intervals {
		if countBuckets(win, iv, loc) <= MaxBuckets {
			return iv.Token
		}
	}
	return intervals[len(intervals)-1].Token
}

// countBuckets returns how many canonical buckets win has at iv, but never
// materializes the axis: it is a cheap guard for ResolveInterval, not an axis
// builder. For counts <= MaxBuckets it MUST agree exactly with
// len(BucketStarts(win, iv, loc)).
//
// Why this exists: building the full axis just to take its length (the old
// bucketCount) let a caller-controlled custom window force ~21M time.Time
// allocations and ~10s of CPU before the cap rejected it — an authenticated
// CPU/memory DoS. Counting cheaply means the cap is checked before anything
// large is allocated; BucketStarts is only ever called once the count is known
// to be in-cap.
//
//   - Fixed (sub-day) intervals: exact O(1) arithmetic — ceil(span/Duration)
//     measured from the SNAPPED first bucket, computed with a modulo so a
//     clamped/huge span cannot overflow.
//   - Calendar (1d) intervals: step by local date like BucketStarts, but stop
//     the moment the count passes MaxBuckets so an absurd span can't walk
//     centuries.
//
// The fixed branch must measure from fixedAxisStart, not from win.Start: an
// off-grid window's axis begins at the grid cell CONTAINING the start, so it can
// span one more bucket than the raw duration implies (an exactly-24h window
// starting at 11:05 touches 25 hourly cells, not 24). Counting from the raw
// start under-reports by that bucket, which would apply MaxBuckets to a count
// the response does not actually have and break the agreement contract above.
func countBuckets(win Window, iv Interval, loc *time.Location) int {
	if loc == nil {
		loc = time.UTC
	}
	if !win.Stop.After(win.Start) {
		return 0
	}

	if !iv.Calendar {
		// Fixed step: ceil(span/Duration) from the snapped first bucket.
		// time.Time.Sub clamps an enormous range to the max Duration, so avoid
		// the (span+Duration-1) form that would overflow and compute ceil via a
		// modulo instead.
		first := fixedAxisStart(win.Start.In(loc), iv, loc)
		span := win.Stop.Sub(first)
		n := span / iv.Duration
		if span%iv.Duration != 0 {
			n++
		}
		return int(n)
	}

	// Calendar: step by local date, anchored at the local midnight of the start
	// date, exactly as BucketStarts does — but bail out once over the cap.
	start := win.Start.In(loc)
	cur := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, loc)
	n := 0
	for cur.Before(win.Stop) {
		n++
		if n > MaxBuckets {
			return n // enough to fail the cap; don't keep walking
		}
		cur = time.Date(cur.Year(), cur.Month(), cur.Day()+1, 0, 0, 0, 0, loc)
	}
	return n
}

// sortedTokens is a stable helper for any caller that wants the allowed tokens
// ordered; kept for symmetry with statehouse helpers.
func sortedTokens() []string {
	out := AllowedIntervals()
	sort.Strings(out)
	return out
}
