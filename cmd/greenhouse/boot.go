package main

import (
	"fmt"

	"github.com/sweeney/greenhouse/internal/config"
)

// validateBootConfig enforces the secure-by-default boundary at startup.
//
// Inbound auth is disabled when identity.base_url is empty (the deliberate
// local-dev/test path). That is fine for dev, but it must never happen by
// accident in production: a single missing or typo'd identity.base_url would
// otherwise bring the whole data API up unauthenticated. So an empty
// identity.base_url is only permitted when auth.allow_insecure is explicitly
// set; otherwise greenhouse refuses to start.
func validateBootConfig(cfg config.Config) error {
	if cfg.Identity.BaseURL == "" && !cfg.Auth.AllowInsecure {
		return fmt.Errorf("identity.base_url is empty and auth.allow_insecure is not set; " +
			"the data API would be unauthenticated. Set identity.base_url, or set " +
			"auth.allow_insecure: true to run without auth (dev only)")
	}
	return nil
}
