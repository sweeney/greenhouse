package climate

import (
	"fmt"
	"strconv"
	"time"
)

// Window labels returned by ResolveWindow.
const (
	WindowToday  = "today"
	WindowWeek   = "week"
	WindowMonth  = "month"
	WindowCustom = "custom"
)

// MaxCustomWindow caps the span of a custom window. The caller controls
// from/to with no inherent limit, so an absurd range (e.g. 200 years) is
// rejected here with an intelligible message before any interval/bucket logic
// runs — defense in depth alongside the O(1) bucket-count cap. ~2 years matches
// the statehouse bucket's Influx retention, beyond which there is no data to
// query anyway.
const MaxCustomWindow = 2 * 366 * 24 * time.Hour

// Window is a resolved, half-open time range [Start, Stop). Label records which
// spec produced it ("today"/"week"/"month"/"custom").
//
// Boundary semantics:
//   - today, week and month are PERIOD-TO-DATE: Start is a local-midnight
//     boundary (today's midnight / most-recent-Monday midnight / the 1st of the
//     month) and Stop is the caller-supplied "now". These windows therefore
//     normally cover a partial day.
//   - custom is an arbitrary explicit range: Start and Stop are exactly the
//     caller-supplied from/to.
type Window struct {
	Start, Stop time.Time
	Label       string
}

// ResolveWindow turns a window spec into a half-open [Start, Stop) range.
//
// It is pure and clock-driven: now is supplied by the caller (sourced from the
// injected Clock) and ResolveWindow never calls time.Now() itself. loc is the
// timezone the calendar boundaries are computed in (Europe/London in prod);
// week starts Monday.
//
// Spec handling:
//   - "today": Start = local midnight of now's date in loc; Stop = now.
//   - "week":  Start = most recent Monday 00:00 local (if now is a Monday, that
//     is today's midnight); Stop = now.
//   - "month": Start = the 1st of now's month at 00:00 local; Stop = now.
//   - "custom": requires both from and to non-zero; Start = from, Stop = to.
//     Errors if either is missing or if to <= from.
//   - "<N>d" (rolling days, e.g. "7d", "30d"): a trailing window of N calendar
//     days ending now, DAY-ALIGNED to local midnight. Start = local midnight of
//     N-1 days ago, Stop = now. So "7d" is today plus the previous 6 full days
//     (7 day-buckets, today partial), and "1d" is equivalent to "today". Label
//     echoes the spec token.
//   - "<N>h" (rolling hours, e.g. "24h"): an EXACT trailing window of N hours.
//     Start = now - N hours, Stop = now. Not midnight-aligned (unlike the day
//     form) — intended for short spans where a fixed look-back is wanted.
//   - any other spec is an error.
//
// Local midnights are built with time.Date(y, m, d, 0,0,0,0, loc) so DST is
// handled correctly: a London "day" may be 23h (spring forward) or 25h (autumn
// back) across the clock change, and the resulting Start carries the correct
// UTC offset for its date (00:00 local is 23:00 UTC in BST, 00:00 UTC in GMT).
func ResolveWindow(now time.Time, loc *time.Location, spec string, from, to time.Time) (Window, error) {
	if loc == nil {
		return Window{}, fmt.Errorf("climate: nil location")
	}

	switch spec {
	case WindowToday:
		ln := now.In(loc)
		start := time.Date(ln.Year(), ln.Month(), ln.Day(), 0, 0, 0, 0, loc)
		return Window{Start: start, Stop: now, Label: WindowToday}, nil

	case WindowWeek:
		ln := now.In(loc)
		midnight := time.Date(ln.Year(), ln.Month(), ln.Day(), 0, 0, 0, 0, loc)
		// Weekday(): Sunday=0..Saturday=6. Days since Monday: Mon->0 .. Sun->6.
		offset := (int(midnight.Weekday()) + 6) % 7
		start := time.Date(ln.Year(), ln.Month(), ln.Day()-offset, 0, 0, 0, 0, loc)
		return Window{Start: start, Stop: now, Label: WindowWeek}, nil

	case WindowMonth:
		ln := now.In(loc)
		start := time.Date(ln.Year(), ln.Month(), 1, 0, 0, 0, 0, loc)
		return Window{Start: start, Stop: now, Label: WindowMonth}, nil

	case WindowCustom:
		if from.IsZero() || to.IsZero() {
			return Window{}, fmt.Errorf("climate: custom window requires both from and to")
		}
		if !to.After(from) {
			return Window{}, fmt.Errorf("climate: custom window to (%s) must be after from (%s)", to, from)
		}
		if to.Sub(from) > MaxCustomWindow {
			return Window{}, fmt.Errorf("climate: custom window span %s exceeds the maximum of %s", to.Sub(from), MaxCustomWindow)
		}
		return Window{Start: from, Stop: to, Label: WindowCustom}, nil

	default:
		if win, ok, err := resolveRolling(now, loc, spec); ok {
			return win, err
		}
		return Window{}, fmt.Errorf("climate: unknown window spec %q", spec)
	}
}

// resolveRolling resolves a trailing-duration spec of the form "<N>d" or "<N>h"
// (N a positive integer) into a Window. The bool reports whether spec was a
// rolling spec at all: false means "not rolling" so the caller falls through to
// its unknown-spec error; true with a non-nil error means "rolling, but invalid"
// (over the span cap).
//
//   - "<N>d": day-aligned to local midnight. Start is the local midnight N-1 days
//     before now's date, so the window spans N calendar-day buckets ending with
//     today's partial day. Built with time.Date so DST and month/year rollover
//     are handled correctly (a London day across a clock change is 23h/25h).
//   - "<N>h": an exact trailing span, Start = now - N hours, not midnight-aligned.
//
// The span is capped at MaxCustomWindow: a rolling window derives its own range,
// so an absurd N (e.g. "100000d") is rejected here with the same "exceeds the
// maximum" message as a custom window, before any interval/bucket logic runs. N
// is bounded against the cap BEFORE the date/duration arithmetic so a huge value
// cannot overflow time.Duration.
func resolveRolling(now time.Time, loc *time.Location, spec string) (Window, bool, error) {
	if len(spec) < 2 {
		return Window{}, false, nil
	}
	unit := spec[len(spec)-1]
	n, err := strconv.Atoi(spec[:len(spec)-1])
	if err != nil || n < 1 {
		return Window{}, false, nil
	}

	maxHours := int64(MaxCustomWindow / time.Hour)
	tooLarge := func() (Window, bool, error) {
		return Window{}, true, fmt.Errorf("climate: rolling window %q exceeds the maximum span of %s", spec, MaxCustomWindow)
	}

	var start time.Time
	switch unit {
	case 'd':
		if int64(n) > maxHours/24 {
			return tooLarge()
		}
		ln := now.In(loc)
		start = time.Date(ln.Year(), ln.Month(), ln.Day()-(n-1), 0, 0, 0, 0, loc)
	case 'h':
		if int64(n) > maxHours {
			return tooLarge()
		}
		start = now.Add(-time.Duration(n) * time.Hour)
	default:
		return Window{}, false, nil
	}
	return Window{Start: start, Stop: now, Label: spec}, true, nil
}

// Days returns the window length in days, computed as the real elapsed duration
// (Stop - Start) divided by 24 hours. This is FRACTIONAL: period-to-date
// windows (today/week/month) end at "now" and so are partial days. Because the
// duration is wall-clock elapsed time, a London day that spans a DST change
// counts as 23h/24 or 25h/24 of a day.
func (w Window) Days() float64 {
	return w.Stop.Sub(w.Start).Hours() / 24
}
