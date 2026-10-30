package config

import (
	"strings"
	"testing"
)

const audienceBase = "site:\n  id: home\n  devices_namespace: devices_home\n"

// Warn is the default: it changes only logging, so it can ship before anyone
// has read the warnings, and moving to enforce is a reviewed change in git.
func TestLoadDefaultsAudienceCheckToWarn(t *testing.T) {
	cfg, err := Load(writeConfig(t, audienceBase))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.AudienceCheck != AudienceWarn {
		t.Errorf("AudienceCheck = %q, want %q", cfg.HTTP.AudienceCheck, AudienceWarn)
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
