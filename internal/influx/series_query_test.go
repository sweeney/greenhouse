package influx

import (
	"strings"
	"testing"
	"time"
)

var (
	seriesStart = time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC)
	seriesStop  = time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC)
)

// The field series builder MUST stamp buckets at their LEFT edge
// (timeSrc:"_start"). Influx's aggregateWindow defaults to the right edge
// (_stop); with the canonical axis keyed on left edges, the default shifts
// every value one bucket late (the first bucket reads empty and the last
// bucket's data is lost). This bit countinghouse; it must not be reintroduced.
func TestBuildFieldSeriesFlux_StampLeftEdge(t *testing.T) {
	for _, fn := range []string{"mean", "min", "max", "last"} {
		flux := BuildFieldSeriesFlux("b", []string{"d"}, "temperature_c", fn, seriesStart, seriesStop, "6h", "Europe/London")
		if !strings.Contains(flux, `timeSrc: "_start"`) {
			t.Errorf("fn=%s: field series builder must set timeSrc:\"_start\" (left-edge buckets); flux:\n%s", fn, flux)
		}
	}
}

func TestBuildFieldSeriesFlux(t *testing.T) {
	flux := BuildFieldSeriesFlux("statehouse", []string{"climate_basement", "climate_weatherstation"}, "temperature_c", "mean", seriesStart, seriesStop, "1h", "Europe/London")

	wants := []string{
		`import "timezone"`,
		`from(bucket: "statehouse")`,
		`r._measurement == "device_environment"`,
		`r._field == "temperature_c"`,
		`contains(value: r.device_id, set: ["climate_basement", "climate_weatherstation"])`,
		`aggregateWindow(every: 1h, fn: mean, timeSrc: "_start", location: timezone.location(name: "Europe/London"), createEmpty: true)`,
		// No pad for a gauge series: range starts AT the window start.
		`start: 2026-06-11T00:00:00Z`,
		`stop: 2026-06-12T00:00:00Z`,
	}
	for _, w := range wants {
		if !strings.Contains(flux, w) {
			t.Errorf("field series flux missing %q\n---\n%s", w, flux)
		}
	}

	// Climate path must not touch energy concepts.
	for _, bad := range []string{`energy_kwh`, `power_w`, `increase()`, `difference()`, `integral(`, `device_power`} {
		if strings.Contains(flux, bad) {
			t.Errorf("field series flux unexpectedly contains %q (energy concept)", bad)
		}
	}
}

func TestBuildFieldSeriesFlux_FieldAndFnParameterised(t *testing.T) {
	cases := []struct {
		field, fn string
	}{
		{"humidity_pct", "min"},
		{"pressure_hpa", "max"},
		{"wind_speed_ms", "last"},
		{"illuminance_lux", "mean"},
	}
	for _, c := range cases {
		flux := BuildFieldSeriesFlux("statehouse", []string{"x"}, c.field, c.fn, seriesStart, seriesStop, "30m", "Europe/London")
		if !strings.Contains(flux, `r._field == "`+c.field+`"`) {
			t.Errorf("field %s not embedded\n---\n%s", c.field, flux)
		}
		if !strings.Contains(flux, "fn: "+c.fn+",") {
			t.Errorf("fn %s not embedded\n---\n%s", c.fn, flux)
		}
		if !strings.Contains(flux, "every: 30m,") {
			t.Errorf("interval token missing\n---\n%s", flux)
		}
	}
}

func TestBuildLatestFlux(t *testing.T) {
	flux := BuildLatestFlux("statehouse", "climate_weatherstation", "7d")
	wants := []string{
		`from(bucket: "statehouse")`,
		`range(start: -7d)`,
		`r._measurement == "device_environment"`,
		`r.device_id == "climate_weatherstation"`,
		`group(columns: ["_field"])`,
		`last()`,
	}
	for _, w := range wants {
		if !strings.Contains(flux, w) {
			t.Errorf("latest flux missing %q\n---\n%s", w, flux)
		}
	}
}

// TestBuildLatestFlux_BoundedRange pins the performance fix: the latest query
// must NOT scan the device's whole history from the Unix epoch. An unbounded
// range defeats time-pruning and makes the scan cost scale with the 2-year
// retention instead of with recency, on a dashboard-polled endpoint.
func TestBuildLatestFlux_BoundedRange(t *testing.T) {
	flux := BuildLatestFlux("statehouse", "d", DefaultLatestLookback)
	if strings.Contains(flux, "1970-01-01") {
		t.Errorf("latest flux must not range from the Unix epoch:\n%s", flux)
	}
	if !strings.Contains(flux, "range(start: -"+DefaultLatestLookback+")") {
		t.Errorf("latest flux should use a bounded lookback range(start: -%s):\n%s", DefaultLatestLookback, flux)
	}
}

func TestDeviceSet(t *testing.T) {
	if got := deviceSet([]string{"a", "b", "c"}); got != `["a", "b", "c"]` {
		t.Errorf("deviceSet = %q", got)
	}
	if got := deviceSet([]string{"only"}); got != `["only"]` {
		t.Errorf("deviceSet single = %q", got)
	}
	if got := deviceSet(nil); got != `[]` {
		t.Errorf("deviceSet empty = %q", got)
	}
}
