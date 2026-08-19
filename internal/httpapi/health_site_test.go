package httpapi

import (
	"encoding/json"
	"testing"
)

// Which property an instance believes it serves is currently only observable by
// inference — read remote_config's keys and deduce the namespace — and the site id is
// not observable at all, because nothing reads it.
//
// That is the wrong way round for the misconfiguration the site block makes possible.
// A second instance pointed at the wrong namespace charts another property's sensors
// while looking entirely healthy, so the config it resolved has to be legible on the
// endpoint an operator already checks, not deducible from whether a chart looks right.
func TestHealthReportsTheResolvedSite(t *testing.T) {
	s, _ := dataSetup(t)
	s.SiteID = "cottage"
	s.DevicesNamespace = "devices_cottage"

	var resp struct {
		Site struct {
			ID               string `json:"id"`
			DevicesNamespace string `json:"devices_namespace"`
		} `json:"site"`
	}
	if err := json.Unmarshal(doGET(t, s, "/healthz").Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Site.ID != "cottage" {
		t.Errorf("healthz site.id = %q, want cottage", resp.Site.ID)
	}
	if resp.Site.DevicesNamespace != "devices_cottage" {
		t.Errorf("healthz site.devices_namespace = %q, want devices_cottage",
			resp.Site.DevicesNamespace)
	}
}

// An instance with no site configured predates the split and is not misconfigured, so
// it says nothing rather than reporting an empty site that reads as a fault.
func TestHealthOmitsAnUnconfiguredSite(t *testing.T) {
	s, _ := dataSetup(t)

	var resp map[string]any
	if err := json.Unmarshal(doGET(t, s, "/healthz").Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if _, ok := resp["site"]; ok {
		t.Errorf("healthz reports a site block when none is configured: %v", resp["site"])
	}
}

// The floorplan namespace is reported for the same reason the devices namespace
// is, and answers a question remote_config cannot: that block distinguishes
// "configured and failing" from "configured and fine" only AFTER a fetch
// attempt, so an operator seeing blank floor names on /floors otherwise cannot
// tell "not configured" from "configured, first fetch hasn't landed" without
// reading the host's config file.
func TestHealthReportsTheFloorplanNamespace(t *testing.T) {
	s, _ := dataSetup(t)
	s.SiteID = "cottage"
	s.DevicesNamespace = "devices_cottage"
	s.FloorplanNamespace = "floorplan_cottage"

	var resp struct {
		Site struct {
			FloorplanNamespace string `json:"floorplan_namespace"`
		} `json:"site"`
	}
	if err := json.Unmarshal(doGET(t, s, "/healthz").Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Site.FloorplanNamespace != "floorplan_cottage" {
		t.Errorf("healthz site.floorplan_namespace = %q, want floorplan_cottage",
			resp.Site.FloorplanNamespace)
	}
}

// The namespace is OPTIONAL, so an instance that sets none must not report an
// empty string that reads as a fault. The key is absent entirely, which is what
// makes "absent means unset" a safe inference for an operator.
func TestHealthOmitsAnUnsetFloorplanNamespace(t *testing.T) {
	s, _ := dataSetup(t)
	s.SiteID = "cottage"
	s.DevicesNamespace = "devices_cottage"
	// FloorplanNamespace deliberately left empty.

	var resp struct {
		Site map[string]any `json:"site"`
	}
	if err := json.Unmarshal(doGET(t, s, "/healthz").Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if v, ok := resp.Site["floorplan_namespace"]; ok {
		t.Errorf("floorplan_namespace = %v present with none configured, want the key absent", v)
	}
	// The rest of the site block is unaffected.
	if resp.Site["devices_namespace"] != "devices_cottage" {
		t.Errorf("devices_namespace = %v, want it unchanged", resp.Site["devices_namespace"])
	}
}

// A floorplan namespace with no site id and no devices namespace is a strange
// config, but it must still surface: reporting nothing would hide the one thing
// that was configured.
func TestHealthReportsAFloorplanNamespaceAlone(t *testing.T) {
	s, _ := dataSetup(t)
	s.FloorplanNamespace = "floorplan_cottage"

	var resp struct {
		Site *struct {
			FloorplanNamespace string `json:"floorplan_namespace"`
		} `json:"site"`
	}
	if err := json.Unmarshal(doGET(t, s, "/healthz").Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Site == nil {
		t.Fatal("site block omitted entirely, hiding the configured floorplan namespace")
	}
	if resp.Site.FloorplanNamespace != "floorplan_cottage" {
		t.Errorf("floorplan_namespace = %q, want floorplan_cottage", resp.Site.FloorplanNamespace)
	}
}
