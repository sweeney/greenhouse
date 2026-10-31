package config

import (
	"context"
	"net/http"
	"testing"
)

// The devices namespace is read with the shared schema (common/devices), field
// by field: a value that does not fit costs that field, not the snapshot.
// Before, one bad value anywhere made the whole document fail to decode, and
// greenhouse kept its last-known snapshot, missing every later change.
func TestFetcher_BadValueCostsOnlyThatField(t *testing.T) {
	mux := http.NewServeMux()
	serveNamespace(mux, testNamespace, map[string]any{
		"sensor_a": map[string]any{"class": "environmental_sensor", "room": "ground.hall"},
		"probe_b": map[string]any{
			"class":              "appliance_probe",
			"room":               "ground.kitchen",
			"target_temperature": "cold",
		},
	})

	f := newTestFetcher(t, mux, &staticTokenSource{token: "test-token"})
	f.Refresh(context.Background())

	if st := f.Statuses()[testNamespace]; !st.OK {
		t.Fatalf("%s status = %+v, want ok", testNamespace, st)
	}
	got := f.Devices()
	if got["sensor_a"].Room != "ground.hall" {
		t.Errorf("sensor_a = %+v", got["sensor_a"])
	}
	p := got["probe_b"]
	if p.Class != "appliance_probe" || p.Room != "ground.kitchen" || p.TargetTemperature != nil {
		t.Errorf("probe_b = %+v, want everything but the bad target_temperature", p)
	}
}
