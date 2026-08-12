package config

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// Removing the fallback is only half the job. The value was defaulted in two places —
// Load and Fetcher.devicesNamespace — and that duplication is the shape of bug that has
// recurred throughout this migration. Both have to go together, and the Fetcher has to
// refuse rather than quietly requesting /api/v1/config/ with an empty name, which would
// swap one silent failure for another.

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// Load must no longer invent a namespace. "Unset" has to survive parsing so that boot
// validation can see it; defaulting is what made the mistake invisible.
func TestLoadDoesNotInventADevicesNamespace(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"site block with id only", "site:\n  id: home\n"},
		{"scalar site", "site: home\n"},
		{"no site block at all", "http:\n  listen: \":8686\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeCfg(t, tc.body))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Site.DevicesNamespace != "" {
				t.Errorf("Load invented a namespace %q; unset must stay unset so boot "+
					"validation can refuse it", cfg.Site.DevicesNamespace)
			}
		})
	}
}

func TestLoadKeepsAnExplicitNamespace(t *testing.T) {
	cfg, err := Load(writeCfg(t, "site:\n  id: home\n  devices_namespace: devices_home\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Site.DevicesNamespace != "devices_home" {
		t.Errorf("DevicesNamespace = %q, want devices_home", cfg.Site.DevicesNamespace)
	}
}

// An unnamed namespace is now a boot error, so warning about it too would be noise
// telling the operator to fix something that already stopped the process.
func TestUnnamedNamespaceIsNotAlsoAWarning(t *testing.T) {
	cfg, err := Load(writeCfg(t, "site:\n  id: home\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Warnings()) != 0 {
		t.Errorf("unnamed namespace should be a boot error, not a warning: %v", cfg.Warnings())
	}
}

// The mirror case stays a warning: that config works, and only costs observability.
func TestNamespaceWithoutSiteIDStillWarns(t *testing.T) {
	cfg, err := Load(writeCfg(t, "site:\n  devices_namespace: devices_home\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Warnings()) == 0 {
		t.Error("a namespace with no site id must still warn: it cannot report which property it serves")
	}
}

// A Fetcher with no namespace must not issue a request at all. Requesting the empty
// name would hit /api/v1/config/ — a different endpoint, failing for a reason that
// tells the operator nothing about the actual mistake.
func TestFetcherRefusesAnEmptyNamespaceRatherThanRequestingIt(t *testing.T) {
	var requested []string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		requested = append(requested, r.URL.Path)
		http.Error(w, "not found", http.StatusNotFound)
	})

	f := newTestFetcher(t, mux, &staticTokenSource{token: "test-token"})
	f.DevicesNamespace = ""
	f.Refresh(context.Background())

	if len(requested) != 0 {
		t.Errorf("Fetcher requested %v with no namespace configured; it must refuse instead", requested)
	}
}
