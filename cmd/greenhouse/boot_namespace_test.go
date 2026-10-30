package main

import (
	"strings"
	"testing"

	"github.com/sweeney/greenhouse/internal/config"
)

// The shared `statehouse_devices` document was deleted from the config service on
// 2026-08-12, so the fallback it backed now points at a 404. Every layer below is
// individually correct and the combination is silent: the fetch 404s, fail-open keeps
// the last-known snapshot (empty at startup), /healthz reports ok because a failing
// namespace is only degraded and there is no last-known-good to be missing, and every
// endpoint honestly reports zero devices. Nothing says "you did not name a namespace".
//
// Refusing to boot is the only layer that can say it, because it is the only one that
// runs before the ambiguity exists.

func baseConfig() config.Config {
	return config.Config{
		Identity: config.IdentityConfig{BaseURL: "https://id.swee.net"},
		Site:     config.SiteConfig{ID: "home", DevicesNamespace: "devices_home"},
	}
}

func TestBootRefusesWhenDevicesNamespaceIsUnnamed(t *testing.T) {
	cfg := baseConfig()
	cfg.Site.DevicesNamespace = ""

	err := validateBootConfig(cfg)
	if err == nil {
		t.Fatal("an unnamed devices_namespace must refuse to boot: the fallback it " +
			"relied on is a 404, so the service would serve zero devices and look healthy")
	}
	// The operator has to be able to act on this without reading the source.
	for _, want := range []string{"devices_namespace", "site"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name %q so it is actionable, got: %v", want, err)
		}
	}
}

// The mirror case still boots: it works, and only costs observability.
func TestBootAllowsNamespaceWithoutSiteID(t *testing.T) {
	cfg := baseConfig()
	cfg.Site.ID = ""

	if err := validateBootConfig(cfg); err != nil {
		t.Errorf("a namespace with no site id must still boot (it works): %v", err)
	}
}

func TestBootAllowsAFullyNamedSite(t *testing.T) {
	if err := validateBootConfig(baseConfig()); err != nil {
		t.Errorf("a fully named site must boot: %v", err)
	}
}

// A name config would refuse never reaches it: remoteconfig rejects it locally and
// sends nothing. At runtime that is one more fail-open warning over an empty
// snapshot, so the name is checked here instead, where it can stop the boot.
func TestBootRefusesANamespaceNameConfigCannotServe(t *testing.T) {
	cases := []struct {
		field string
		set   func(*config.Config)
	}{
		{"devices_namespace", func(c *config.Config) { c.Site.DevicesNamespace = "Devices.Home" }},
		{"floorplan_namespace", func(c *config.Config) { c.Site.FloorplanNamespace = "floor plan" }},
		{"floorplan_namespace", func(c *config.Config) { c.Site.FloorplanNamespace = strings.Repeat("f", 65) }},
	}
	for _, tc := range cases {
		cfg := baseConfig()
		tc.set(&cfg)
		err := validateBootConfig(cfg)
		if err == nil {
			t.Errorf("%s: an invalid name must refuse to boot", tc.field)
			continue
		}
		if !strings.Contains(err.Error(), tc.field) {
			t.Errorf("error must name %q so it is actionable, got: %v", tc.field, err)
		}
	}
}

func TestBootAllowsValidNamespaceNames(t *testing.T) {
	cfg := baseConfig()
	cfg.Site.DevicesNamespace = "devices_home-2"
	cfg.Site.FloorplanNamespace = "floorplan_home"
	if err := validateBootConfig(cfg); err != nil {
		t.Errorf("valid names must boot: %v", err)
	}
}
