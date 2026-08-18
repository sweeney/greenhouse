package config

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// staticTokenSource satisfies TokenSource for tests.
type staticTokenSource struct{ token string }

func (s *staticTokenSource) Token(_ context.Context) (string, error) { return s.token, nil }
func (s *staticTokenSource) Invalidate()                             {}

// trackingTokenSource records whether Invalidate was called.
type trackingTokenSource struct {
	token       string
	invalidated bool
}

func (t *trackingTokenSource) Token(_ context.Context) (string, error) { return t.token, nil }
func (t *trackingTokenSource) Invalidate()                             { t.invalidated = true }

func newTestFetcher(t *testing.T, mux *http.ServeMux, tokens TokenSource) *Fetcher {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Fetcher{
		BaseURL: srv.URL,
		// Named explicitly: there is no default, so a Fetcher without this refuses
		// to fetch at all. Tests that want that behaviour clear it themselves.
		DevicesNamespace: testNamespace,
		Tokens:           tokens,
		HTTPClient:       srv.Client(),
	}
}

// testNamespace stands in for a real per-site namespace. It is deliberately not
// `statehouse_devices`: that document was deleted from the config service, and a test
// naming it would suggest it is still something greenhouse can fall back to.
const testNamespace = "devices_home"

// serveNamespace serves a JSON namespace requiring Bearer test-token.
func serveNamespace(mux *http.ServeMux, ns string, v any) {
	mux.HandleFunc("/api/v1/config/"+ns, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v) //nolint:errcheck
	})
}

func TestFetcher_RefreshPopulatesSnapshot(t *testing.T) {
	mux := http.NewServeMux()
	serveNamespace(mux, testNamespace, map[string]any{
		"probe_a": map[string]any{
			// Legacy Z2M shorthand: normaliseDevices folds these into
			// scheme=zigbee, primary=ieee_address, display=friendly_name.
			"ieee_address":  "0xaabbccddeeff0011",
			"friendly_name": "Glow Sensor",
			"class":         "environmental_sensor",
			"display_name":  "Probe A",
			"location":      "area-d",
		},
	})

	f := newTestFetcher(t, mux, &staticTokenSource{token: "test-token"})
	f.Refresh(context.Background())

	devices := f.Devices()
	d, ok := devices["probe_a"]
	if !ok {
		t.Fatal("probe_a missing after refresh")
	}
	if d.Class != "environmental_sensor" {
		t.Errorf("class: got %q, want environmental_sensor", d.Class)
	}
	if d.Scheme != "zigbee" {
		t.Errorf("scheme: got %q, want zigbee (normalised)", d.Scheme)
	}
	if d.Primary != "0xaabbccddeeff0011" {
		t.Errorf("primary: got %q, want 0xaabbccddeeff0011 (normalised)", d.Primary)
	}

	st := f.Statuses()
	if !st[testNamespace].OK {
		t.Error("status not OK for the configured namespace")
	}
	if st[testNamespace].FetchedAt.IsZero() {
		t.Error("fetched_at is zero for the configured namespace")
	}
}

func TestFetcher_DevicesReturnsCopy(t *testing.T) {
	mux := http.NewServeMux()
	serveNamespace(mux, testNamespace, map[string]any{
		"sensor_a": map[string]any{"class": "environmental_sensor"},
	})

	f := newTestFetcher(t, mux, &staticTokenSource{token: "test-token"})
	f.Refresh(context.Background())

	got := f.Devices()
	got["sensor_a"] = DeviceConfig{Class: "mutated"}
	got["injected"] = DeviceConfig{}

	again := f.Devices()
	if again["sensor_a"].Class != "environmental_sensor" {
		t.Error("mutating returned map leaked into the held snapshot")
	}
	if _, ok := again["injected"]; ok {
		t.Error("injecting into returned map leaked into the held snapshot")
	}
}

func TestFetcher_401InvalidatesAndKeepsSnapshot(t *testing.T) {
	// Phase 1: a healthy server populates the snapshot.
	good := http.NewServeMux()
	serveNamespace(good, testNamespace, map[string]any{
		"sensor_b": map[string]any{"class": "environmental_sensor", "display_name": "Sensor B"},
	})
	goodSrv := httptest.NewServer(good)
	defer goodSrv.Close()

	src := &trackingTokenSource{token: "stale-token"}
	f := &Fetcher{BaseURL: goodSrv.URL, DevicesNamespace: testNamespace,
		Tokens: &staticTokenSource{token: "test-token"}, HTTPClient: goodSrv.Client()}
	f.Refresh(context.Background())
	if _, ok := f.Devices()["sensor_b"]; !ok {
		t.Fatal("precondition: sensor_b should be present after first refresh")
	}

	// Phase 2: point the fetcher at a server that always 401s.
	bad := http.NewServeMux()
	bad.HandleFunc("/api/v1/config/"+testNamespace, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
	badSrv := httptest.NewServer(bad)
	defer badSrv.Close()
	f.BaseURL = badSrv.URL
	f.HTTPClient = badSrv.Client()
	f.Tokens = src

	f.Refresh(context.Background())

	if !src.invalidated {
		t.Error("expected Invalidate() after 401")
	}
	// Fail-open: prior snapshot is retained.
	if _, ok := f.Devices()["sensor_b"]; !ok {
		t.Error("device snapshot was wiped after a 401 (should fail-open)")
	}
	if f.Statuses()[testNamespace].OK {
		t.Error("status should record the 401 failure")
	}
}

func TestFetcher_TokenFailureKeepsSnapshot(t *testing.T) {
	mux := http.NewServeMux()
	serveNamespace(mux, testNamespace, map[string]any{
		"sensor_c": map[string]any{"class": "environmental_sensor"},
	})
	f := newTestFetcher(t, mux, &staticTokenSource{token: "test-token"})
	f.Refresh(context.Background())
	if _, ok := f.Devices()["sensor_c"]; !ok {
		t.Fatal("precondition: sensor_c present after first refresh")
	}

	// Swap in a token source that errors.
	f.Tokens = &errTokenSource{}
	f.Refresh(context.Background())

	if _, ok := f.Devices()["sensor_c"]; !ok {
		t.Error("snapshot wiped after token failure (should fail-open)")
	}
	if f.Statuses()[testNamespace].OK {
		t.Error("status should record token failure")
	}
}

func TestFetcher_EmptyBaseURLNoOp(t *testing.T) {
	f := &Fetcher{Tokens: &errTokenSource{}}
	f.Refresh(context.Background()) // must not panic, must not call Token
	if len(f.Devices()) != 0 {
		t.Error("expected empty devices")
	}
	if len(f.Statuses()) != 0 {
		t.Error("expected no statuses recorded for empty base url")
	}
}

type errTokenSource struct{}

func (errTokenSource) Token(context.Context) (string, error) {
	return "", context.DeadlineExceeded
}
func (errTokenSource) Invalidate() {}
