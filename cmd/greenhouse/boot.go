package main

import (
	"fmt"

	"github.com/sweeney/common/remoteconfig"
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
// It also requires the devices namespace to be named. That check has the same shape:
// a config state that parses fine and must never reach production. It used to fall
// back to the shared `statehouse_devices` document, but that was deleted from the
// config service on 2026-08-12, so the fallback now names a 404. Every layer below
// handles that correctly and the combination is silent — the fetch fails, fail-open
// keeps the last-known snapshot (empty at startup), /healthz reports ok because a
// failing namespace is only degraded and there is no last-known-good to be missing,
// and every endpoint honestly reports zero devices. Boot is the only place that can
// still tell the difference between "unnamed" and "named and empty".
//
// A namespace name the config service would refuse is a boot refusal too, the
// floorplan's included: optional means it may be left unset, not that it may be
// set to something that can never be fetched. remoteconfig rejects such a name
// without sending a request, so at runtime it would be just one more fail-open
// warning over an empty snapshot.
func validateBootConfig(cfg config.Config) error {
	if cfg.Identity.BaseURL == "" && !cfg.Auth.AllowInsecure {
		return fmt.Errorf("identity.base_url is empty and auth.allow_insecure is not set; " +
			"the data API would be unauthenticated. Set identity.base_url, or set " +
			"auth.allow_insecure: true to run without auth (dev only)")
	}
	for _, n := range []struct{ field, name string }{
		{"site.devices_namespace", cfg.Site.DevicesNamespace},
		{"site.floorplan_namespace", cfg.Site.FloorplanNamespace},
	} {
		// Empty is unset, not invalid: the floorplan is optional, and an unset
		// devices namespace gets its own message below.
		if n.name == "" {
			continue
		}
		// remoteconfig.ValidName is the check Get makes before each request, so
		// boot and every fetch apply one rule.
		if err := remoteconfig.ValidName(n.name); err != nil {
			return fmt.Errorf("%s %q is not a name the config service accepts "+
				"(lowercase letters, digits, _ and -, at most 64), so it could never be "+
				"fetched: %w", n.field, n.name, err)
		}
	}
	if cfg.Site.DevicesNamespace == "" {
		return fmt.Errorf("site.devices_namespace is not set, so there is no device " +
			"inventory to read and every endpoint would serve zero devices. Name it " +
			"explicitly, e.g.:\n\nsite:\n  id: home\n  devices_namespace: devices_home")
	}
	return nil
}
