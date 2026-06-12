package httpapi

import (
	"math"
	"net/http"
)

// valueDP is the decimal precision climate values are rounded to at the response
// boundary (matching internal/climate). Two places suits all the gauge units.
const valueDP = 2

// roundTo rounds f to dp decimal places, passing NaN/Inf through untouched.
func roundTo(f float64, dp int) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return f
	}
	p := math.Pow(10, float64(dp))
	return math.Round(f*p) / p
}

// writeError writes a JSON error body with the given status.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
