package climate

import (
	"math"
	"testing"
)

// TestRoundValue exercises the single, exported climate rounding helper that
// both the assembly step and the httpapi latest handler now share. Previously
// roundTo + valueDP were duplicated across internal/climate and internal/httpapi
// (with two cosmetically-different bodies), a copy-drift hazard for a
// correctness-critical precision helper.
func TestRoundValue(t *testing.T) {
	if ValueDP != 2 {
		t.Fatalf("ValueDP = %d, want 2", ValueDP)
	}
	cases := []struct {
		in, want float64
	}{
		{21.456, 21.46},
		{21.454, 21.45},
		{0.125, 0.13}, // half away from zero
		{-0.125, -0.13},
		{1.0, 1.0},
		{-2.005, -2.01}, // float repr of -2.005 is just below, rounds away to -2.01
	}
	for _, c := range cases {
		if got := RoundValue(c.in); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("RoundValue(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestRoundValue_NaNInfPassthrough confirms NaN/Inf pass through untouched so
// empty buckets stay gaps (they must not be rounded to a real number).
func TestRoundValue_NaNInfPassthrough(t *testing.T) {
	if !math.IsNaN(RoundValue(math.NaN())) {
		t.Error("RoundValue(NaN) should stay NaN")
	}
	if !math.IsInf(RoundValue(math.Inf(1)), 1) {
		t.Error("RoundValue(+Inf) should stay +Inf")
	}
	if !math.IsInf(RoundValue(math.Inf(-1)), -1) {
		t.Error("RoundValue(-Inf) should stay -Inf")
	}
}
