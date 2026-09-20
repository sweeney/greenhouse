package httpapi

import (
	"encoding/json"
	"net/http"
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
