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

// publishedFloorplanDoc is the shape config.swee.net ACTUALLY publishes: a
// single "floors" key holding an array, each record carrying its own id, and
// extra keys (here "ceiling") greenhouse does not model.
//
// The map-keyed form above was assumed from how the devices namespace is shaped.
// The floorplan namespace is not shaped that way, so greenhouse decoded nothing
// and fail-open turned a wrong guess into blank names in prod — the fetch failed
// with "cannot unmarshal array into Go value of type config.FloorConfig", /floors
// degraded to UNKNOWN, and it looked exactly like a namespace that was never
// configured.
func publishedFloorplanDoc() map[string]any {
	return map[string]any{
		"floors": []any{
			map[string]any{"id": "floor1", "name": "Lower Floor", "order": 1, "elevation": 0.0, "ceiling": 2.5},
			map[string]any{"id": "floor2", "name": "Upper Floor", "order": 2, "elevation": 3.2, "ceiling": 2.5},
			map[string]any{"id": "floor3", "name": "Top Floor", "ceiling": 2.5},
		},
	}
}

func TestFetcher_RefreshPopulatesFloorsFromPublishedShape(t *testing.T) {
	mux := http.NewServeMux()
	serveNamespace(mux, testNamespace, map[string]any{
		"sensor_e": map[string]any{"class": "environmental_sensor", "floor": "floor1"},
	})
	serveNamespace(mux, testFloorplanNamespace, publishedFloorplanDoc())

	f := floorplanFetcher(t, mux)
	f.Refresh(context.Background())

	floors := f.Floors()
	if len(floors) != 3 {
		t.Fatalf("got %d floors, want 3: %v", len(floors), floors)
	}
	if got := floors["floor1"].Name; got != "Lower Floor" {
		t.Errorf("floor1 name = %q, want Lower Floor", got)
	}
	if o := floors["floor2"].Order; o == nil || *o != 2 {
		t.Errorf("floor2 order = %v, want 2", o)
	}
	if e := floors["floor2"].Elevation; e == nil || *e != 3.2 {
		t.Errorf("floor2 elevation = %v, want 3.2", e)
	}
	// The inline id is what devices reference and floors= matches, so it must
	// survive into the record rather than being left empty.
	if got := floors["floor3"].ID; got != "floor3" {
		t.Errorf("floor3 id = %q, want floor3", got)
	}
	if o := floors["floor3"].Order; o != nil {
		t.Errorf("floor3 declares no order, so it must stay UNKNOWN, got %v", *o)
	}

	// A namespace this fetch understood must report healthy, or /healthz says
	// "ok" while the data behind it is missing.
	if st := f.Statuses()[testFloorplanNamespace]; !st.OK {
		t.Errorf("floorplan status not ok: %+v", st)
	}
}

// The two shapes are told apart by JSON type, not by key name: a floor whose id
// is literally "floors" is an OBJECT there, so the document is the map shape and
// that floor decodes normally. Pinned because keying off the name alone would
// silently drop it.
func TestFetcher_FloorNamedFloorsIsNotMistakenForTheArrayShape(t *testing.T) {
	mux := http.NewServeMux()
	serveNamespace(mux, testNamespace, map[string]any{})
	serveNamespace(mux, testFloorplanNamespace, map[string]any{
		"floors": map[string]any{"name": "Oddly Named Floor", "order": 1},
	})

	f := floorplanFetcher(t, mux)
	f.Refresh(context.Background())

	got := f.Floors()
	if len(got) != 1 || got["floors"].Name != "Oddly Named Floor" {
		t.Fatalf("want the map shape decoded, got %v", got)
	}
}

// In the array shape the id is inline, and a record without one cannot be
// referenced by a device's `floor` or matched by floors=. It is skipped rather
// than keyed on "", which would publish a floor nothing can select.
func TestFetcher_ArrayShapeSkipsRecordsWithNoID(t *testing.T) {
	mux := http.NewServeMux()
	serveNamespace(mux, testNamespace, map[string]any{})
	serveNamespace(mux, testFloorplanNamespace, map[string]any{
		"floors": []any{
			map[string]any{"id": "floor1", "name": "Lower Floor"},
			map[string]any{"name": "Nameless", "order": 2},
		},
	})

	f := floorplanFetcher(t, mux)
	f.Refresh(context.Background())

	got := f.Floors()
	if len(got) != 1 {
		t.Fatalf("want only the identified floor, got %v", got)
	}
	if _, ok := got[""]; ok {
		t.Error(`a floor was keyed on "", which floors= can never match`)
	}
}

// The floorplan document carries the building's ROOMS as well as its floors, in
// the same wrapper-of-arrays shape (#28). Rooms are what /rooms publishes and
// what labels a group_by=room series.

// publishedFloorplanWithRooms is the full published document: both collections,
// each an array of records carrying their own ids, with unmodelled keys.
func publishedFloorplanWithRooms() map[string]any {
	return map[string]any{
		"floors": []any{
			map[string]any{"id": "floor1", "name": "Lower Floor", "order": 1, "ceiling": 2.5},
			map[string]any{"id": "floor2", "name": "Upper Floor", "order": 2, "ceiling": 2.5},
		},
		"rooms": []any{
			map[string]any{
				"id": "floor1.room-a", "name": "Room A", "floor": "floor1",
				"category": "utility", "area": 12.4, "polygon": []any{1, 2, 3},
			},
			map[string]any{
				"id": "floor1.room-c", "name": "Room C", "floor": "floor1",
				"category": "plant", "area": 1.3,
			},
			// Declares no category or area: UNKNOWN, not guessed.
			map[string]any{"id": "floor2.room-a", "name": "Room A", "floor": "floor2"},
		},
	}
}

func TestFetcher_RefreshPopulatesRooms(t *testing.T) {
	mux := http.NewServeMux()
	serveNamespace(mux, testNamespace, map[string]any{
		"sensor_e": map[string]any{"class": "environmental_sensor", "floor": "floor1"},
	})
	serveNamespace(mux, testFloorplanNamespace, publishedFloorplanWithRooms())

	f := floorplanFetcher(t, mux)
	f.Refresh(context.Background())

	rooms := f.Rooms()
	if len(rooms) != 3 {
		t.Fatalf("got %d rooms, want 3: %v", len(rooms), rooms)
	}
	a := rooms["floor1.room-a"]
	if a.Name != "Room A" || a.Floor != "floor1" || a.Category != "utility" {
		t.Errorf("floor1.room-a = %+v, want the declared name/floor/category", a)
	}
	if a.Area == nil || *a.Area != 12.4 {
		t.Errorf("floor1.room-a area = %v, want 12.4", a.Area)
	}
	// The category that motivated the request: a plant room, relayed raw.
	if got := rooms["floor1.room-c"].Category; got != "plant" {
		t.Errorf("floor1.room-c category = %q, want plant relayed unchanged", got)
	}
	// Declares neither: UNKNOWN on both counts.
	if r := rooms["floor2.room-a"]; r.Category != "" || r.Area != nil {
		t.Errorf("floor2.room-a = %+v, want category empty and area nil", r)
	}
	// Both collections arrive from the one document.
	if len(f.Floors()) != 2 {
		t.Errorf("got %d floors, want 2 alongside the rooms", len(f.Floors()))
	}
}

// Two rooms on different floors may share a name, and both must survive: the id
// is the key, so a name collision is not a collision at all.
func TestFetcher_RoomsWithTheSameNameBothSurvive(t *testing.T) {
	mux := http.NewServeMux()
	serveNamespace(mux, testNamespace, map[string]any{})
	serveNamespace(mux, testFloorplanNamespace, publishedFloorplanWithRooms())

	f := floorplanFetcher(t, mux)
	f.Refresh(context.Background())

	rooms := f.Rooms()
	if rooms["floor1.room-a"].Name != rooms["floor2.room-a"].Name {
		t.Fatal("fixture should have two rooms sharing a name")
	}
	if rooms["floor1.room-a"].Floor == rooms["floor2.room-a"].Floor {
		t.Error("the two same-named rooms must keep their distinct floors")
	}
}

// A document publishing floors but no rooms is legitimate — rooms are simply
// UNKNOWN — and must not fail the whole fetch.
func TestFetcher_FloorsWithoutRoomsIsFine(t *testing.T) {
	mux := http.NewServeMux()
	serveNamespace(mux, testNamespace, map[string]any{})
	serveNamespace(mux, testFloorplanNamespace, map[string]any{
		"floors": []any{map[string]any{"id": "floor1", "name": "Lower Floor"}},
	})

	f := floorplanFetcher(t, mux)
	f.Refresh(context.Background())

	if len(f.Floors()) != 1 {
		t.Errorf("got %d floors, want 1", len(f.Floors()))
	}
	if len(f.Rooms()) != 0 {
		t.Errorf("rooms = %v, want empty", f.Rooms())
	}
	if st := f.Statuses()[testFloorplanNamespace]; !st.OK {
		t.Errorf("a floors-only document is a successful fetch: %+v", st)
	}
}

// And the reverse: rooms with no floors. The wrapper is recognised by EITHER
// collection being an array, so a rooms-only document is not mistaken for the
// legacy map shape and does not fail trying to unmarshal an array into a
// FloorConfig — which is exactly how #28's bug presented.
func TestFetcher_RoomsWithoutFloorsIsFine(t *testing.T) {
	mux := http.NewServeMux()
	serveNamespace(mux, testNamespace, map[string]any{})
	serveNamespace(mux, testFloorplanNamespace, map[string]any{
		"rooms": []any{map[string]any{"id": "floor1.room-a", "name": "Room A"}},
	})

	f := floorplanFetcher(t, mux)
	f.Refresh(context.Background())

	if len(f.Rooms()) != 1 {
		t.Fatalf("got %d rooms, want 1: %v", len(f.Rooms()), f.Rooms())
	}
	if st := f.Statuses()[testFloorplanNamespace]; !st.OK {
		t.Errorf("a rooms-only document is a successful fetch: %+v", st)
	}
}

// An id-less room record cannot be referenced by a device's `room` property or
// matched by rooms=, so it is skipped rather than keyed on "" — the same rule
// floors follow.
func TestFetcher_RoomsSkipRecordsWithNoID(t *testing.T) {
	mux := http.NewServeMux()
	serveNamespace(mux, testNamespace, map[string]any{})
	serveNamespace(mux, testFloorplanNamespace, map[string]any{
		"rooms": []any{
			map[string]any{"id": "floor1.room-a", "name": "Room A"},
			map[string]any{"name": "Nameless"},
		},
	})

	f := floorplanFetcher(t, mux)
	f.Refresh(context.Background())

	if len(f.Rooms()) != 1 {
		t.Fatalf("want only the identified room, got %v", f.Rooms())
	}
	if _, ok := f.Rooms()[""]; ok {
		t.Error(`a room was keyed on "", which rooms= can never match`)
	}
}

// The legacy devices-style map shape carries FLOORS ONLY — there is no
// room-shaped reading of it — so rooms stay UNKNOWN rather than being invented
// from floor records.
func TestFetcher_LegacyMapShapeYieldsNoRooms(t *testing.T) {
	mux := http.NewServeMux()
	serveNamespace(mux, testNamespace, map[string]any{})
	serveNamespace(mux, testFloorplanNamespace, floorplanDoc())

	f := floorplanFetcher(t, mux)
	f.Refresh(context.Background())

	if len(f.Floors()) != 3 {
		t.Errorf("got %d floors, want the map shape still decoded", len(f.Floors()))
	}
	if len(f.Rooms()) != 0 {
		t.Errorf("rooms = %v, want empty: the map shape publishes no rooms", f.Rooms())
	}
}

// Rooms() returns a COPY, like Floors(): a caller mutating it must not corrupt
// the snapshot every subsequent request is served from.
func TestFetcher_RoomsReturnsACopy(t *testing.T) {
	mux := http.NewServeMux()
	serveNamespace(mux, testNamespace, map[string]any{})
	serveNamespace(mux, testFloorplanNamespace, publishedFloorplanWithRooms())

	f := floorplanFetcher(t, mux)
	f.Refresh(context.Background())

	got := f.Rooms()
	delete(got, "floor1.room-a")
	got["floor1.room-c"] = RoomConfig{Name: "tampered"}

	fresh := f.Rooms()
	if _, ok := fresh["floor1.room-a"]; !ok {
		t.Error("deleting from the returned map mutated the held snapshot")
	}
	if fresh["floor1.room-c"].Name != "Room C" {
		t.Errorf("floor1.room-c name = %q, want the snapshot untouched",
			fresh["floor1.room-c"].Name)
	}
}

// A failed refresh keeps the last-known ROOMS too, not just floors: they come
// from one document and degrade together.
func TestFetcher_FloorplanFailureKeepsLastKnownRooms(t *testing.T) {
	mux := http.NewServeMux()
	serveNamespace(mux, testNamespace, map[string]any{})
	var fail bool
	mux.HandleFunc("/api/v1/config/"+testFloorplanNamespace, func(w http.ResponseWriter, r *http.Request) {
		if fail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(publishedFloorplanWithRooms()) //nolint:errcheck
	})

	f := floorplanFetcher(t, mux)
	f.Refresh(context.Background())
	if len(f.Rooms()) != 3 {
		t.Fatalf("first refresh: got %d rooms, want 3", len(f.Rooms()))
	}

	fail = true
	f.Refresh(context.Background())

	if len(f.Rooms()) != 3 {
		t.Errorf("got %d rooms after a failed refresh, want the 3 last-known kept",
			len(f.Rooms()))
	}
}

// Concurrent reads of both collections during refresh must be race-free; this
// runs under -race.
func TestFetcher_RoomsConcurrentReadDuringRefresh(t *testing.T) {
	mux := http.NewServeMux()
	serveNamespace(mux, testNamespace, map[string]any{})
	serveNamespace(mux, testFloorplanNamespace, publishedFloorplanWithRooms())

	f := floorplanFetcher(t, mux)
	f.Refresh(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			_, _ = f.Rooms(), f.Floors()
		}
	}()
	for i := 0; i < 50; i++ {
		f.Refresh(context.Background())
	}
	<-done
}
