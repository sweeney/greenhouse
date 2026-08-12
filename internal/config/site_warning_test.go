package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The bare-id spelling is what statehouse has deployed. A parser that rejected it
// would have taken the service down the moment it shipped — which makes it the single
// riskiest claim in this change, and nothing pinned it. A later tidy-up of
// UnmarshalYAML could have broken the deployed spelling with a green suite.
func TestScalarSiteFormKeepsParsing(t *testing.T) {
	cfg, err := Load(writeConfig(t, "site: home\n"))
	if err != nil {
		t.Fatalf("the deployed scalar spelling must keep parsing: %v", err)
	}
	if cfg.Site.ID != "home" {
		t.Errorf("Site.ID = %q, want home", cfg.Site.ID)
	}
	// The scalar form names no namespace, and there is no default left to supply one,
	// so this config parses and is then refused at boot. See boot_namespace_test.go.
	if cfg.Site.DevicesNamespace != "" {
		t.Errorf("DevicesNamespace = %q, want empty: the scalar form names no namespace",
			cfg.Site.DevicesNamespace)
	}
}

// The mirror case: a namespace with no site to own it. The right devices load, but
// /healthz then reports an instance that cannot say which property it serves — the
// question the block exists to answer.
func TestNamespaceWithoutASiteIDIsWarnedAbout(t *testing.T) {
	cfg, err := Load(writeConfig(t, "site:\n  devices_namespace: devices_home\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Warnings()) == 0 {
		t.Error("a devices_namespace with no site id must warn")
	}
}

// A warning that fires on correct config is noise, and noise gets filtered out — which
// is how the warning above would stop being read before it ever mattered.
func TestCorrectlyConfiguredSitesAreSilent(t *testing.T) {
	for _, tc := range []struct{ name, yaml string }{
		{"both keys given", "site:\n  id: home\n  devices_namespace: devices_home\n"},
		{"no site block at all", "http:\n  listen: \":8080\"\n"},
	} {
		cfg, err := Load(writeConfig(t, tc.yaml))
		if err != nil {
			t.Fatalf("%s: load: %v", tc.name, err)
		}
		if w := cfg.Warnings(); len(w) != 0 {
			t.Errorf("%s: want no warnings, got %v", tc.name, w)
		}
	}
}
