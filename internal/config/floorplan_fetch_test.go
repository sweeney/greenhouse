package config

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// The floorplan namespace is OPTIONAL, unlike the devices namespace. Greenhouse
// charts devices; a floor's label and storey order are presentation detail, so a
// missing or broken floorplan must never be able to stop a climate service
// serving climate.

const testFloorplanNamespace = "floorplan_home"

// floorplanDoc is the namespace document shape: keyed by floor id, one record
// each, with floor3 deliberately declaring no order so the UNKNOWN path is
// always exercised.
func floorplanDoc() map[string]any {
	return map[string]any{
		"floor1": map[string]any{"name": "Lower Floor", "order": 1, "elevation": 0.0},
		"floor2": map[string]any{"name": "Upper Floor", "order": 2, "elevation": 3.2},
		"floor3": map[string]any{"name": "Top Floor"},
	}
}

// floorplanFetcher wires a fetcher serving both namespaces.
func floorplanFetcher(t *testing.T, mux *http.ServeMux) *Fetcher {
	t.Helper()
	f := newTestFetcher(t, mux, &staticTokenSource{token: "test-token"})
	f.FloorplanNamespace = testFloorplanNamespace
	return f
}

func TestFetcher_RefreshPopulatesFloors(t *testing.T) {
	mux := http.NewServeMux()
	serveNamespace(mux, testNamespace, map[string]any{
		"sensor_e": map[string]any{"class": "environmental_sensor", "floor": "floor1"},
	})
	serveNamespace(mux, testFloorplanNamespace, floorplanDoc())

	f := floorplanFetcher(t, mux)
	f.Refresh(context.Background())

	floors := f.Floors()
	if len(floors) != 3 {
		t.Fatalf("got %d floors, want 3: %v", len(floors), floors)
	}
	if got := floors["floor1"].Name; got != "Lower Floor" {
		t.Errorf("floor1 name = %q, want the declared label", got)
	}
	if o := floors["floor2"].Order; o == nil || *o != 2 {
		t.Errorf("floor2 order = %v, want 2", o)
	}
	// Normalised from the key, so the id is usable as a floors= value.
	if got := floors["floor3"].ID; got != "floor3" {
		t.Errorf("floor3 ID = %q, want it filled from the key", got)
	}
	// Declares no order: UNKNOWN, not zero.
	if o := floors["floor3"].Order; o != nil {
		t.Errorf("floor3 order = %v, want nil", *o)
	}
}

// Fetching floors must not disturb the devices snapshot, which is the thing the
// service actually needs.
func TestFetcher_FloorplanDoesNotDisturbDevices(t *testing.T) {
	mux := http.NewServeMux()
	serveNamespace(mux, testNamespace, map[string]any{
		"sensor_e": map[string]any{"class": "environmental_sensor", "floor": "floor1"},
	})
	serveNamespace(mux, testFloorplanNamespace, floorplanDoc())

	f := floorplanFetcher(t, mux)
	f.Refresh(context.Background())

	if len(f.Devices()) != 1 {
		t.Errorf("devices = %v, want the one fetched device", f.Devices())
	}
}

// An unconfigured floorplan namespace is silent: no request, no status, no
// warning. There is nothing configured to be unhealthy about.
func TestFetcher_NoFloorplanNamespaceIsSilent(t *testing.T) {
	mux := http.NewServeMux()
	serveNamespace(mux, testNamespace, map[string]any{
		"sensor_e": map[string]any{"class": "environmental_sensor"},
	})
	var floorplanRequested bool
	mux.HandleFunc("/api/v1/config/"+testFloorplanNamespace, func(w http.ResponseWriter, _ *http.Request) {
		floorplanRequested = true
		w.WriteHeader(http.StatusOK)
	})

	f := newTestFetcher(t, mux, &staticTokenSource{token: "test-token"})
	// FloorplanNamespace deliberately left empty.
	f.Refresh(context.Background())

	if floorplanRequested {
		t.Error("no floorplan namespace is configured, so nothing should have been requested")
	}
	if len(f.Floors()) != 0 {
		t.Errorf("floors = %v, want empty", f.Floors())
	}
	if _, ok := f.Statuses()[testFloorplanNamespace]; ok {
		t.Error("an unconfigured namespace must record no status: there is nothing to be unhealthy")
	}
}

// A failing floorplan namespace is FAIL-OPEN and, crucially, must not prevent
// the devices snapshot from being served. Floor labels are presentation detail.
func TestFetcher_FloorplanFailureKeepsDevicesServing(t *testing.T) {
	mux := http.NewServeMux()
	serveNamespace(mux, testNamespace, map[string]any{
		"sensor_e": map[string]any{"class": "environmental_sensor", "floor": "floor1"},
	})
	mux.HandleFunc("/api/v1/config/"+testFloorplanNamespace, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})

	f := floorplanFetcher(t, mux)
	f.Refresh(context.Background())

	if len(f.Devices()) != 1 {
		t.Errorf("devices = %v: a floorplan outage must not cost the device snapshot", f.Devices())
	}
	if len(f.Floors()) != 0 {
		t.Errorf("floors = %v, want empty after a failed fetch", f.Floors())
	}
	// But an operator who ASKED for floor records can see they are not arriving.
	st, ok := f.Statuses()[testFloorplanNamespace]
	if !ok {
		t.Fatal("a configured namespace that failed must record a status")
	}
	if st.OK {
		t.Error("status should report not-ok after a 500")
	}
}

// Fail-open means the last-known-good snapshot survives a later failure, so a
// transient config-service outage does not blank the floor labels.
func TestFetcher_FloorplanFailureKeepsLastKnownFloors(t *testing.T) {
	mux := http.NewServeMux()
	serveNamespace(mux, testNamespace, map[string]any{
		"sensor_e": map[string]any{"class": "environmental_sensor"},
	})
	var fail bool
	mux.HandleFunc("/api/v1/config/"+testFloorplanNamespace, func(w http.ResponseWriter, r *http.Request) {
		if fail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(floorplanDoc()) //nolint:errcheck
	})

	f := floorplanFetcher(t, mux)
	f.Refresh(context.Background())
	if len(f.Floors()) != 3 {
		t.Fatalf("first refresh: got %d floors, want 3", len(f.Floors()))
	}

	fail = true
	f.Refresh(context.Background())

	if len(f.Floors()) != 3 {
		t.Errorf("got %d floors after a failed refresh, want the 3 last-known kept",
			len(f.Floors()))
	}
	if got := f.Floors()["floor1"].Name; got != "Lower Floor" {
		t.Errorf("floor1 name = %q, want the last-known label", got)
	}
}

// An empty namespace document is a legitimate answer: no floor records, which is
// UNKNOWN for every floor rather than an error.
func TestFetcher_EmptyFloorplanDocument(t *testing.T) {
	mux := http.NewServeMux()
	serveNamespace(mux, testNamespace, map[string]any{
		"sensor_e": map[string]any{"class": "environmental_sensor"},
	})
	serveNamespace(mux, testFloorplanNamespace, map[string]any{})

	f := floorplanFetcher(t, mux)
	f.Refresh(context.Background())

	if len(f.Floors()) != 0 {
		t.Errorf("floors = %v, want empty", f.Floors())
	}
	if st, ok := f.Statuses()[testFloorplanNamespace]; !ok || !st.OK {
		t.Error("an empty document is a successful fetch, not a failure")
	}
}

// Floors() returns a COPY: a caller mutating it must not corrupt the snapshot
// every subsequent request is served from.
func TestFetcher_FloorsReturnsACopy(t *testing.T) {
	mux := http.NewServeMux()
	serveNamespace(mux, testNamespace, map[string]any{
		"sensor_e": map[string]any{"class": "environmental_sensor"},
	})
	serveNamespace(mux, testFloorplanNamespace, floorplanDoc())

	f := floorplanFetcher(t, mux)
	f.Refresh(context.Background())

	got := f.Floors()
	delete(got, "floor1")
	got["floor2"] = FloorConfig{Name: "tampered"}

	fresh := f.Floors()
	if _, ok := fresh["floor1"]; !ok {
		t.Error("deleting from the returned map mutated the held snapshot")
	}
	if fresh["floor2"].Name != "Upper Floor" {
		t.Errorf("floor2 name = %q, want the snapshot untouched", fresh["floor2"].Name)
	}
}

// Concurrent reads during a refresh must be race-free; this runs under -race.
func TestFetcher_FloorsConcurrentReadDuringRefresh(t *testing.T) {
	mux := http.NewServeMux()
	serveNamespace(mux, testNamespace, map[string]any{
		"sensor_e": map[string]any{"class": "environmental_sensor"},
	})
	serveNamespace(mux, testFloorplanNamespace, floorplanDoc())

	f := floorplanFetcher(t, mux)
	f.Refresh(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			_ = f.Floors()
		}
	}()
	for i := 0; i < 50; i++ {
		f.Refresh(context.Background())
	}
	<-done
}
