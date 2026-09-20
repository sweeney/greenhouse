package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sweeney/greenhouse/internal/config"
	"github.com/sweeney/greenhouse/internal/influx"
)

// The API half of the class split. climate's tests pin what the combine does;
// these pin what the endpoints advertise — that a probe is discoverable and
// selectable, and that neither catalog offers it as a room or a floor.

// probeAPIDevices mirrors the prod kitchen: an ambient sensor and a freezer
// probe in ONE room, plus a probe alone in a room of its own so the
// probe-only case is observable from outside.
func probeAPIDevices() map[string]config.DeviceConfig {
	return map[string]config.DeviceConfig{
		"climate_kitchen": {
			Class: "environmental_sensor", Room: "groundfloor.kitchen", Floor: "groundfloor",
			DisplayName: "Climate: Kitchen", EnvironmentFields: []string{"temperature_c", "humidity_pct"},
		},
		"probe_freezer": {
			Class: "appliance_probe", Room: "groundfloor.kitchen", Floor: "groundfloor",
			DisplayName: "Probe: Kitchen Freezer", EnvironmentFields: []string{"temperature_c"},
		},
		// Alone in its room and on its floor: nothing ambient anywhere near it.
		"probe_winefridge": {
			Class: "appliance_probe", Room: "cellar.store", Floor: "cellar",
			DisplayName: "Probe: Wine Fridge", EnvironmentFields: []string{"temperature_c"},
		},
	}
}

func probeAPISetup(t *testing.T) (*Server, *influx.FakeQuerier) {
	t.Helper()
	s, q := dataSetup(t)
	s.Config = fakeConfig{devices: probeAPIDevices()}
	return s, q
}

// A probe must be in the catalog, or no picker can offer it and the readings
// are invisible however healthy the sensor is. This is the bug the split fixes.
func TestDevices_CatalogIncludesApplianceProbes(t *testing.T) {
	s, _ := probeAPISetup(t)
	resp := getCatalog(t, s)

	if len(resp.Devices) != 3 {
		t.Fatalf("want all 3 climate devices, got %d: %+v", len(resp.Devices), resp.Devices)
	}
	var found bool
	for _, d := range resp.Devices {
		if d.ID != "probe_freezer" {
			continue
		}
		found = true
		// The class is reported as-is so a consumer can tell a probe from an
		// ambient sensor — which is the handle it needs to label them apart.
		if d.Class != "appliance_probe" {
			t.Errorf("class = %q, want appliance_probe reported as-is", d.Class)
		}
		if d.Room != "groundfloor.kitchen" {
			t.Errorf("room = %q, want the appliance's room", d.Room)
		}
	}
	if !found {
		t.Error("probe_freezer missing from /devices — a probe that is charted must be discoverable")
	}
}

// devices= must accept a probe: it is the selector behind group_by=device, the
// only grouping that charts one.
func TestSeries_DevicesFilterAcceptsAnApplianceProbe(t *testing.T) {
	s, _ := probeAPISetup(t)

	w := doGET(t, s, "/series?devices=probe_freezer&window=today")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
}

// The single-device convenience path must agree with /series about what a
// climate sensor is, or moving between them turns a chart into a 400.
func TestDeviceSeries_AcceptsAnApplianceProbe(t *testing.T) {
	s, _ := probeAPISetup(t)

	w := doGET(t, s, "/devices/probe_freezer/series?window=today")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
}

// /rooms is the vocabulary behind group_by=room, so it must list a room when and
// only when grouping yields a series for it. cellar.store holds nothing but a
// probe, so grouping produces nothing and the catalog must not offer it.
func TestRooms_ExcludesAProbeOnlyRoom(t *testing.T) {
	s, _ := probeAPISetup(t)
	got := getRooms(t, s)

	if _, ok := roomByID(got, "cellar.store"); ok {
		t.Error("cellar.store holds only an appliance probe — it has no ambient reading and must not be listed as a room")
	}
	r, ok := roomByID(got, "groundfloor.kitchen")
	if !ok {
		t.Fatalf("groundfloor.kitchen missing from %v", got)
	}
	// The kitchen holds two climate devices but only ONE that describes the
	// room, and the count must say so rather than promising an average of two.
	if r.DeviceCount != 1 {
		t.Errorf("device_count = %d, want 1 — the probe is charted but never combined into the room", r.DeviceCount)
	}
}

// Same rule one axis up.
func TestFloors_ExcludesAProbeOnlyFloor(t *testing.T) {
	s, _ := probeAPISetup(t)

	w := doGET(t, s, "/floors")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Floors []struct {
			ID          string `json:"id"`
			DeviceCount int    `json:"device_count"`
		} `json:"floors"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %s: %v", w.Body.String(), err)
	}
	for _, f := range resp.Floors {
		if f.ID == "cellar" {
			t.Error("the cellar holds only an appliance probe — it must not be listed as a floor")
		}
		if f.ID == "groundfloor" && f.DeviceCount != 1 {
			t.Errorf("groundfloor device_count = %d, want 1 (probe excluded from the floor combine)", f.DeviceCount)
		}
	}
}

// --- target_temperature ---
//
// The namespace declares what an appliance is meant to be holding; greenhouse
// relays it so a consumer does not have to read config.swee.net itself, or
// hardcode a setpoint, to know what the probe beside it should be showing.

func f64(v float64) *float64 { return &v }

// targetDevices covers all three cases the field has to tell apart: a declared
// target, a declared target of ZERO, and no target at all.
func targetDevices() map[string]config.DeviceConfig {
	return map[string]config.DeviceConfig{
		"probe_freezer": {
			Class: "appliance_probe", Room: "groundfloor.kitchen", Floor: "groundfloor",
			DisplayName: "Probe: Freezer", EnvironmentFields: []string{"temperature_c"},
			TargetTemperature: f64(-18),
		},
		"probe_chiller": {
			Class: "appliance_probe", Room: "groundfloor.kitchen", Floor: "groundfloor",
			DisplayName: "Probe: Chiller", EnvironmentFields: []string{"temperature_c"},
			TargetTemperature: f64(0), // a real target, not an absent one
		},
		"climate_kitchen": {
			Class: "environmental_sensor", Room: "groundfloor.kitchen", Floor: "groundfloor",
			DisplayName: "Climate: Kitchen", EnvironmentFields: []string{"temperature_c"},
			// no target declared
		},
	}
}

// catalogTargets decodes /devices keeping the raw JSON for each entry, so the
// difference between `null` and an absent key is observable.
func catalogTargets(t *testing.T, s *Server) map[string]json.RawMessage {
	t.Helper()
	w := doGET(t, s, "/devices")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Devices []map[string]json.RawMessage `json:"devices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %s: %v", w.Body.String(), err)
	}
	out := map[string]json.RawMessage{}
	for _, d := range resp.Devices {
		var id string
		_ = json.Unmarshal(d["id"], &id)
		out[id] = d["target_temperature"]
	}
	return out
}

func TestDevices_RelaysDeclaredTargetTemperature(t *testing.T) {
	s, _ := dataSetup(t)
	s.Config = fakeConfig{devices: targetDevices()}
	got := catalogTargets(t, s)

	if string(got["probe_freezer"]) != "-18" {
		t.Errorf("freezer target = %s, want -18 relayed from the namespace", got["probe_freezer"])
	}
}

// The reason the field is a pointer. A freezer chiller held at 0 °C has a real
// target; serialised as a bare float it would be indistinguishable from one with
// no target at all, and a consumer would draw a 0 °C line for every device.
func TestDevices_ZeroTargetIsNotAbsent(t *testing.T) {
	s, _ := dataSetup(t)
	s.Config = fakeConfig{devices: targetDevices()}
	got := catalogTargets(t, s)

	if string(got["probe_chiller"]) != "0" {
		t.Errorf("chiller target = %s, want 0 — an explicit 0 °C target must survive", got["probe_chiller"])
	}
	if string(got["climate_kitchen"]) != "null" {
		t.Errorf("kitchen target = %s, want null — no declared target", got["climate_kitchen"])
	}
}

// The key is always present, so a consumer can read "no target published" from
// null rather than having to distinguish a missing field from a parse failure.
func TestDevices_TargetKeyAlwaysPresent(t *testing.T) {
	s, _ := dataSetup(t)
	s.Config = fakeConfig{devices: targetDevices()}
	got := catalogTargets(t, s)

	for _, id := range []string{"probe_freezer", "probe_chiller", "climate_kitchen"} {
		if _, ok := got[id]; !ok || len(got[id]) == 0 {
			t.Errorf("%s has no target_temperature key at all; it must be present and null when undeclared", id)
		}
	}
}

// greenhouse relays the number and draws no conclusion from it: no in-range
// flag, no tolerance, no breach count. Whether 6.8 °C against a target of 5 is a
// problem is the consumer's policy, exactly as a room's category is.
func TestDevices_TargetCarriesNoVerdict(t *testing.T) {
	s, _ := dataSetup(t)
	s.Config = fakeConfig{devices: targetDevices()}
	w := doGET(t, s, "/devices")
	body := w.Body.String()

	for _, banned := range []string{"in_range", "on_target", "within_target", "target_band", "tolerance", "breach"} {
		if strings.Contains(body, banned) {
			t.Errorf("catalog exposes %q — greenhouse relays the target, it does not judge against it", banned)
		}
	}
}

// --- the documented chartable-vs-ambient asymmetry ---
//
// `rooms=`/`floors=` validate against the CHARTABLE set while /rooms and /floors
// list the AMBIENT one, so a probe-only room is accepted by the filter yet
// absent from the catalog. The README and the spec both promise this gap runs in
// exactly one direction — the catalog is never WIDER than the filter — because
// that is what stops a picker built from the catalog producing a 400.

// The filter accepts a probe-only room even though /rooms does not list it.
func TestSeries_RoomsFilterAcceptsAProbeOnlyRoom(t *testing.T) {
	s, _ := probeAPISetup(t)

	w := doGET(t, s, "/series?rooms=cellar.store&window=today")
	if w.Code != http.StatusOK {
		t.Fatalf("rooms=cellar.store: want 200 (it holds a chartable probe), got %d: %s", w.Code, w.Body.String())
	}
}

func TestSeries_FloorsFilterAcceptsAProbeOnlyFloor(t *testing.T) {
	s, _ := probeAPISetup(t)

	w := doGET(t, s, "/series?floors=cellar&window=today")
	if w.Code != http.StatusOK {
		t.Fatalf("floors=cellar: want 200 (it holds a chartable probe), got %d: %s", w.Code, w.Body.String())
	}
}

// ...and grouping that room yields NO series rather than one keyed on a freezer
// interior. 200-with-nothing, not a 400 and not an invented ambient reading.
func TestSeries_GroupByRoomOnAProbeOnlyRoomIsEmpty(t *testing.T) {
	s, _ := probeAPISetup(t)

	w := doGET(t, s, "/series?rooms=cellar.store&group_by=room&window=today")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Series []struct {
			Key string `json:"key"`
		} `json:"series"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %s: %v", w.Body.String(), err)
	}
	if len(resp.Series) != 0 {
		t.Errorf("want no series for a room with no ambient member, got %d: %+v", len(resp.Series), resp.Series)
	}
}

// The direction that must never reverse: every room the catalog lists has to be
// one the filter accepts. If this ever fails, a picker built from /rooms can
// produce a 400 — the exact failure both catalogs exist to prevent.
func TestRooms_CatalogIsNeverWiderThanTheFilter(t *testing.T) {
	s, _ := probeAPISetup(t)

	for _, r := range getRooms(t, s) {
		w := doGET(t, s, "/series?rooms="+r.ID+"&window=today")
		if w.Code != http.StatusOK {
			t.Errorf("/rooms lists %q but rooms=%s returns %d — the catalog must never be wider than the filter",
				r.ID, r.ID, w.Code)
		}
	}
}
