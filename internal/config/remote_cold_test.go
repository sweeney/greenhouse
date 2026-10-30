package config

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// Devices are cold until they land once, and stale (not cold) after that: boot
// needs truth, running keeps the last truth.
func TestFetcher_DevicesColdUntilTheyLand(t *testing.T) {
	healthy := true
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/config/"+testNamespace, func(w http.ResponseWriter, r *http.Request) {
		if !healthy {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{"sensor_a":{"class":"environmental_sensor"}}`)) //nolint:errcheck
	})
	f := newTestFetcher(t, mux, &staticTokenSource{token: "test-token"})

	if got := f.Cold(); !reflect.DeepEqual(got, []string{testNamespace}) {
		t.Fatalf("before any refresh, Cold() = %v, want [%s]", got, testNamespace)
	}

	healthy = false
	f.Refresh(context.Background())
	if got := f.Cold(); !reflect.DeepEqual(got, []string{testNamespace}) {
		t.Fatalf("after a failed first refresh, Cold() = %v, want [%s]", got, testNamespace)
	}

	healthy = true
	f.Refresh(context.Background())
	if got := f.Cold(); got != nil {
		t.Fatalf("after devices landed, Cold() = %v, want nil", got)
	}

	healthy = false
	f.Refresh(context.Background())
	if got := f.Cold(); got != nil {
		t.Fatalf("a failure after landing is stale, not cold; Cold() = %v", got)
	}
}

// The floorplan is presentation detail and optional: failing, or never landing,
// must never make greenhouse cold.
func TestFetcher_FloorplanNeverMakesItCold(t *testing.T) {
	mux := http.NewServeMux()
	serveNamespace(mux, testNamespace, map[string]any{
		"sensor_a": map[string]any{"class": "environmental_sensor"},
	})
	mux.HandleFunc("/api/v1/config/floorplan_home", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such namespace", http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	f := &Fetcher{BaseURL: srv.URL, Tokens: &staticTokenSource{token: "test-token"},
		HTTPClient: srv.Client(), DevicesNamespace: testNamespace, FloorplanNamespace: "floorplan_home"}
	f.Refresh(context.Background())

	if f.Statuses()["floorplan_home"].OK {
		t.Fatal("precondition: the floorplan fetch should have failed")
	}
	if got := f.Cold(); got != nil {
		t.Errorf("Cold() = %v; a failing floorplan must never make greenhouse cold", got)
	}
}
