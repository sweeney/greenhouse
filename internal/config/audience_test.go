package config

import (
	"strings"
	"testing"
)

const audienceBase = "site:\n  id: home\n  devices_namespace: devices_home\n"

// Enforce is the default: warn ran in production first and logged no caller
// that enforcing would lock out (#39). warn and off stay available
// as an explicit setting, for a rollback without a rebuild.
func TestLoadDefaultsAudienceCheckToEnforce(t *testing.T) {
	cfg, err := Load(writeConfig(t, audienceBase))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.AudienceCheck != AudienceEnforce {
		t.Errorf("AudienceCheck = %q, want %q", cfg.HTTP.AudienceCheck, AudienceEnforce)
	}
}

func TestLoadAcceptsEachAudienceMode(t *testing.T) {
	for _, mode := range []string{AudienceWarn, AudienceEnforce, AudienceOff} {
		cfg, err := Load(writeConfig(t, audienceBase+"http:\n  audience_check: "+mode+"\n"))
		if err != nil {
			t.Errorf("%s: %v", mode, err)
			continue
		}
		if cfg.HTTP.AudienceCheck != mode {
			t.Errorf("AudienceCheck = %q, want %q", cfg.HTTP.AudienceCheck, mode)
		}
	}
}

// A misspelt mode must not quietly mean off.
func TestLoadRefusesAnUnknownAudienceMode(t *testing.T) {
	_, err := Load(writeConfig(t, audienceBase+"http:\n  audience_check: enforcing\n"))
	if err == nil || !strings.Contains(err.Error(), "http.audience_check") {
		t.Fatalf("Load = %v, want an error naming http.audience_check", err)
	}
}
