package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sweeney/greenhouse/internal/config"
)

// A greenhouse whose devices have never been fetched would serve zero devices
// while /healthz said ok. At boot that is refused, naming the namespace.
func TestColdBootIsRefusedNamingTheDevicesNamespace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	f := &config.Fetcher{BaseURL: srv.URL, HTTPClient: srv.Client(), DevicesNamespace: "devices_home"}
	f.Refresh(context.Background())

	err := requireWarmDevices(f)
	if err == nil {
		t.Fatal("requireWarmDevices = nil with devices never fetched; want a refusal")
	}
	if !strings.Contains(err.Error(), "devices_home") {
		t.Errorf("refusal %q must name the namespace that never arrived", err)
	}
}

func TestWarmBootIsAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"sensor_a":{"class":"environmental_sensor"}}`)) //nolint:errcheck
	}))
	defer srv.Close()
	f := &config.Fetcher{BaseURL: srv.URL, HTTPClient: srv.Client(), DevicesNamespace: "devices_home"}
	f.Refresh(context.Background())

	if err := requireWarmDevices(f); err != nil {
		t.Errorf("requireWarmDevices = %v after devices landed; want nil", err)
	}
}
