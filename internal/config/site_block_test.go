package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The devices namespace is named by config rather than hardcoded, so a site can have
// its own. Publishing a per-site namespace does nothing while every service fetches a
// fixed name — which is exactly what happened.
func TestSiteBlockNamesTheDevicesNamespace(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	body := "site:\n  id: home\n  devices_namespace: devices_home\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Site.ID != "home" {
		t.Errorf("Site.ID = %q, want home", cfg.Site.ID)
	}
	if cfg.Site.DevicesNamespace != "devices_home" {
		t.Errorf("Site.DevicesNamespace = %q, want devices_home", cfg.Site.DevicesNamespace)
	}
}

// The no-default replacements for the two cases this file used to cover live in
// namespace_required_test.go: Load must leave an unnamed namespace unset, and the
// Fetcher must refuse one rather than requesting the empty name. That the Fetcher
// reads the name it is given is covered end-to-end by remote_test.go.
