package config

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// A hot water cylinder sends no readings itself; its probes do. The namespace
// declares it with `telemetry: false` and no identity, and each probe names it
// through `part_of` (#47). That is a deliberate shape, not a misconfiguration, so
// a devices refresh must not warn identity_missing for it — nor anything else for
// the cylinder or its probes — and the probes must still come through as they did.
func TestFetcher_TelemetryFalseEntryDoesNotWarnIdentityMissing(t *testing.T) {
	mux := http.NewServeMux()
	serveNamespace(mux, testNamespace, map[string]any{
		"hw_cylinder": map[string]any{
			"class":     "hot_water_cylinder",
			"telemetry": false,
			"room":      "ground.utility",
			"floor":     "ground",
		},
		"hw_cylinder_top": map[string]any{
			"class":   "appliance_probe",
			"scheme":  "tanktemp",
			"primary": "hw_cylinder.top",
			"part_of": "hw_cylinder",
			"room":    "ground.utility",
			"floor":   "ground",
		},
	})

	var logged bytes.Buffer
	f := newTestFetcher(t, mux, &staticTokenSource{token: "test-token"})
	f.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	f.Refresh(context.Background())

	out := logged.String()
	if strings.Contains(out, "identity_missing") {
		t.Errorf("a telemetry:false entry must not warn identity_missing; got:\n%s", out)
	}
	if strings.Contains(out, "devices problem") {
		t.Errorf("the cylinder and its probe are well-formed and must log no devices problem; got:\n%s", out)
	}

	probe, ok := f.Devices()["hw_cylinder_top"]
	if !ok {
		t.Fatal("hw_cylinder_top missing after refresh")
	}
	if probe.Class != "appliance_probe" || probe.Scheme != "tanktemp" ||
		probe.Primary != "hw_cylinder.top" || probe.Room != "ground.utility" {
		t.Errorf("probe = %+v, want it decoded unchanged", probe)
	}
	if !f.Statuses()[testNamespace].OK {
		t.Error("status not OK for the devices namespace")
	}
}
