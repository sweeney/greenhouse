package config

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// TokenSource is satisfied by auth.TokenSource. The Fetcher uses it to obtain a
// Bearer token for the config service and to invalidate that token on a 401 so
// the next fetch retries with a fresh credential.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
	// Invalidate clears any cached token. Called by Fetcher when the config
	// service responds with 401, so the next Token() call fetches a fresh one
	// rather than replaying the rejected credential for its full TTL.
	Invalidate()
}

// NamespaceStatus records the outcome of the most recent fetch attempt for a
// single config namespace, surfaced on /healthz.
type NamespaceStatus struct {
	OK        bool      `json:"ok"`
	FetchedAt time.Time `json:"fetched_at"`
	Error     string    `json:"error,omitempty"`
}

// Fetcher fetches the devices namespace greenhouse depends on — named by
// cfg.Site.DevicesNamespace, which is required and has no default — and HOLDS it
// as a live snapshot. It also fetches the OPTIONAL floorplan namespace
// (cfg.Site.FloorplanNamespace) when one is configured.
//
// Greenhouse is read-side and stateless: the Fetcher is the authoritative
// in-memory view that the HTTP handlers query via the ConfigProvider interface
// (Devices()). main.go refreshes it once at startup and again on SIGHUP.
//
// Unlike countinghouse, greenhouse fetches NO tariffs — it is a climate
// service, not an energy one.
//
// Refresh is FAIL-OPEN: any error (token, transport, non-200, decode) is logged
// as a warning and the last-known-good snapshot is kept. This keeps the service
// serving the last good config across transient config-service outages, which
// matches the stateless invariant (truth is rebuildable; never lose good data).
type Fetcher struct {
	BaseURL    string
	Tokens     TokenSource
	HTTPClient *http.Client
	Logger     *slog.Logger

	// DevicesNamespace names the namespace holding this site's devices. Required:
	// there is no default document left to fall back to, so an empty value makes
	// Refresh a no-op with a warning rather than a request for the empty name.
	DevicesNamespace string

	// FloorplanNamespace names the namespace holding this site's floor records.
	// OPTIONAL: an empty value is not an error and records no status — greenhouse
	// simply holds no floor records and reports every floor's name and order as
	// UNKNOWN. It is presentation detail, so it must never be able to stop a
	// climate service serving climate.
	FloorplanNamespace string

	mu       sync.RWMutex
	devices  map[string]DeviceConfig
	floors   map[string]FloorConfig
	statuses map[string]NamespaceStatus
}

// defaultFetchClient is used when Fetcher.HTTPClient is nil.
var defaultFetchClient = &http.Client{Timeout: 10 * time.Second}

// maxConfigBytes is the maximum response body size accepted from the config service.
const maxConfigBytes = 1 << 20 // 1 MiB

// Devices returns a copy of the current devices snapshot keyed by
// device_id. Safe for concurrent use. Implements httpapi.ConfigProvider.
func (f *Fetcher) Devices() map[string]DeviceConfig {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make(map[string]DeviceConfig, len(f.devices))
	for k, v := range f.devices {
		out[k] = v
	}
	return out
}

// Floors returns a copy of the current floor-record snapshot keyed by floor id.
// Safe for concurrent use. Implements httpapi.FloorProvider.
//
// An empty map means either that no floorplan namespace is configured or that
// none has been fetched yet. Both are UNKNOWN rather than "there are no floors":
// callers enrich what they have and report the rest empty, never inferring that
// a floor does not exist because its record is missing.
func (f *Fetcher) Floors() map[string]FloorConfig {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make(map[string]FloorConfig, len(f.floors))
	for k, v := range f.floors {
		out[k] = v
	}
	return out
}

// Statuses returns a snapshot of the last fetch result for each namespace.
// Implements httpapi.ConfigStatus.
func (f *Fetcher) Statuses() map[string]NamespaceStatus {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make(map[string]NamespaceStatus, len(f.statuses))
	for k, v := range f.statuses {
		out[k] = v
	}
	return out
}

// Refresh fetches the configured devices namespace and swaps the held snapshot
// on success.
// Fail-open: on any error the previous snapshot is kept and a per-namespace
// status is recorded. Refresh never panics.
func (f *Fetcher) Refresh(ctx context.Context) {
	if !strings.HasPrefix(f.BaseURL, "https://") && f.BaseURL != "" {
		f.warn("remote config: base_url is not https — bearer token will be transmitted in cleartext", "url", f.BaseURL)
	}
	if f.BaseURL == "" {
		// No remote config configured; serve empty snapshots. Don't attempt a
		// token fetch or record a status — there is nothing to fetch.
		return
	}
	if f.DevicesNamespace == "" {
		// There is no default to fall back to. Requesting the empty name would ask
		// for /api/v1/config/ — a different endpoint, failing for a reason that says
		// nothing about the actual mistake. main.go refuses to boot in this state, so
		// reaching here means a hand-built Fetcher; say so rather than guessing.
		f.warn("remote config: no devices namespace configured, not fetching")
		return
	}
	token, err := f.Tokens.Token(ctx)
	if err != nil {
		f.warn("remote config: identity token fetch failed, keeping last-known snapshot", "error", err)
		f.recordStatus(f.DevicesNamespace, err)
		return
	}
	f.refreshDevices(ctx, token)
	f.refreshFloorplan(ctx, token)
}

// refreshFloorplan fetches the optional floorplan namespace.
//
// Fail-open like refreshDevices, and additionally OPTIONAL: an unset namespace
// is silent and records no status, because there is nothing configured to be
// unhealthy about. A configured-but-failing namespace does record one, so an
// operator who asked for floor records can see they are not arriving — but it
// still never blocks the devices snapshot or the endpoints that matter.
func (f *Fetcher) refreshFloorplan(ctx context.Context, token string) {
	if f.FloorplanNamespace == "" {
		return
	}
	var doc floorplanDocument
	if err := f.fetch(ctx, token, f.FloorplanNamespace, &doc); err != nil {
		f.warn("remote config: floorplan namespace unavailable, keeping last-known",
			"namespace", f.FloorplanNamespace, "error", err)
		f.recordStatus(f.FloorplanNamespace, err)
		return
	}
	floors := map[string]FloorConfig(doc)
	if floors == nil {
		floors = map[string]FloorConfig{}
	}
	normaliseFloors(floors)
	f.mu.Lock()
	f.floors = floors
	f.mu.Unlock()
	f.recordStatus(f.FloorplanNamespace, nil)
}

func (f *Fetcher) refreshDevices(ctx context.Context, token string) {
	var devices map[string]DeviceConfig
	if err := f.fetch(ctx, token, f.DevicesNamespace, &devices); err != nil {
		f.warn("remote config: devices namespace unavailable, keeping last-known",
			"namespace", f.DevicesNamespace, "error", err)
		f.recordStatus(f.DevicesNamespace, err)
		return
	}
	if devices == nil {
		devices = map[string]DeviceConfig{}
	}
	normaliseDevices(devices)
	f.mu.Lock()
	f.devices = devices
	f.mu.Unlock()
	f.recordStatus(f.DevicesNamespace, nil)
}

func (f *Fetcher) recordStatus(ns string, err error) {
	s := NamespaceStatus{OK: err == nil, FetchedAt: time.Now()}
	if err != nil {
		s.Error = err.Error()
	}
	f.mu.Lock()
	if f.statuses == nil {
		f.statuses = make(map[string]NamespaceStatus)
	}
	f.statuses[ns] = s
	f.mu.Unlock()
}

func (f *Fetcher) fetch(ctx context.Context, token, ns string, dst any) error {
	client := f.HTTPClient
	if client == nil {
		client = defaultFetchClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		f.BaseURL+"/api/v1/config/"+ns, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		f.Tokens.Invalidate()
		return fmt.Errorf("unauthorized: token may be stale, invalidated for next retry")
	}
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		return fmt.Errorf("namespace %s: unexpected status %d", ns, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxConfigBytes+1))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if int64(len(body)) > maxConfigBytes {
		return fmt.Errorf("config response exceeds %d bytes", maxConfigBytes)
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("unmarshal: %w", err)
	}
	return nil
}

func (f *Fetcher) warn(msg string, args ...any) {
	if f.Logger != nil {
		f.Logger.Warn(msg, args...)
	}
}
