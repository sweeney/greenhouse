package main

import (
	"testing"

	"github.com/sweeney/greenhouse/internal/config"
)

// TestValidateBootConfig pins the secure-by-default boundary: greenhouse must
// refuse to start when identity.base_url is empty unless auth.allow_insecure is
// explicitly set, so a single missing/typo'd config key can never silently bring
// the whole data API up unauthenticated.
func TestValidateBootConfig(t *testing.T) {
	// The namespace is set on every fixture so these cases keep testing the auth
	// boundary alone; the namespace refusal is covered in boot_namespace_test.go.
	withAuth := func() config.Config {
		c := config.Default()
		c.Identity.BaseURL = "https://id.swee.net"
		c.Site = config.SiteConfig{ID: "home", DevicesNamespace: "devices_home"}
		return c
	}

	t.Run("configured auth boots", func(t *testing.T) {
		if err := validateBootConfig(withAuth()); err != nil {
			t.Errorf("configured auth should boot: %v", err)
		}
	})

	t.Run("empty base_url without opt-in refuses", func(t *testing.T) {
		c := withAuth()
		c.Identity.BaseURL = ""
		if err := validateBootConfig(c); err == nil {
			t.Error("empty identity.base_url without allow_insecure must refuse to start")
		}
	})

	t.Run("empty base_url with explicit opt-in boots", func(t *testing.T) {
		c := withAuth()
		c.Identity.BaseURL = ""
		c.Auth.AllowInsecure = true
		if err := validateBootConfig(c); err != nil {
			t.Errorf("allow_insecure should permit an empty identity.base_url: %v", err)
		}
	})
}
