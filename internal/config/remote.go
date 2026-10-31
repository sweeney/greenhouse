package config

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/sweeney/common/devices"
	"github.com/sweeney/common/remoteconfig"
)

// TokenSource is satisfied by auth.TokenSource. remoteconfig uses it to obtain a
// Bearer token for the config service, and invalidates that token on a 401
// before retrying once with a fresh credential.
type TokenSource = remoteconfig.TokenSource

// NamespaceStatus records the outcome of the most recent fetch attempt for a
// single config namespace, surfaced on /healthz.
type NamespaceStatus = remoteconfig.Status

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
	// UserAgent is sent to the config service, conventionally
	// "greenhouse/<version>". Empty sends remoteconfig's default.
	UserAgent string

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

	mu      sync.RWMutex
	devices map[string]DeviceConfig
	floors  map[string]FloorConfig
	rooms   map[string]RoomConfig
	tracker remoteconfig.Tracker
}

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

// Rooms returns a copy of the current room-record snapshot keyed by floorplan
// room id. Safe for concurrent use. Implements httpapi.FloorplanProvider.
//
// An empty map means no floorplan namespace is configured, none has been fetched
// yet, or the document is the legacy floors-only map shape. All three are
// UNKNOWN rather than "there are no rooms": callers enrich what they have and
// report the rest empty, never inferring that a room does not exist because its
// record is missing.
func (f *Fetcher) Rooms() map[string]RoomConfig {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make(map[string]RoomConfig, len(f.rooms))
	for k, v := range f.rooms {
		out[k] = v
	}
	return out
}

// Cold returns the devices namespace if it has never been fetched successfully,
// or nil once it has landed.
//
// Cold is the state fail-open cannot cover: with nothing ever fetched there is
// no last-known snapshot to keep, and greenhouse would serve zero devices in the
// confident shape of a correct answer while /healthz said ok. So main.go refuses
// to boot on a non-empty result. It is a query rather than an error from Refresh
// because the two callers want opposite things: startup must refuse, and a
// SIGHUP reload must not, since by then a snapshot exists and a later failure is
// merely stale.
//
// Only devices count. The floorplan is presentation detail and optional, so its
// never landing must not stop a climate service serving climate.
func (f *Fetcher) Cold() []string {
	return f.tracker.Cold(f.DevicesNamespace)
}

// Statuses returns a snapshot of the last fetch result for each namespace.
// Implements httpapi.ConfigStatus.
func (f *Fetcher) Statuses() map[string]NamespaceStatus {
	return f.tracker.Statuses()
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
	if f.Tokens != nil {
		// One token serves every namespace: remoteconfig asks for it per request,
		// and the TokenSource answers from its cache. Asking here first means a
		// token that cannot be had costs identity one call rather than one per
		// namespace, and that every namespace records the real cause. Otherwise a
		// hung identity would spend the whole refresh deadline on devices, and
		// floorplan would record only the deadline.
		if _, err := f.Tokens.Token(ctx); err != nil {
			err = fmt.Errorf("remote config token: %w", err)
			for _, ns := range []string{f.DevicesNamespace, f.FloorplanNamespace} {
				if ns != "" {
					f.warn("remote config: namespace unavailable, keeping last-known",
						"namespace", ns, "error", err)
					f.tracker.Record(ns, err)
				}
			}
			return
		}
	}
	// Built once per Refresh, so one refresh reads every namespace with the
	// same settings.
	c := &remoteconfig.Client{
		BaseURL:    f.BaseURL,
		Tokens:     f.Tokens,
		UserAgent:  f.UserAgent,
		HTTPClient: f.HTTPClient,
	}
	f.refreshDevices(ctx, c)
	f.refreshFloorplan(ctx, c)
}

// refreshFloorplan fetches the optional floorplan namespace, which carries both
// the building's floors and its rooms.
//
// Fail-open like refreshDevices, and additionally OPTIONAL: an unset namespace
// is silent and records no status, because there is nothing configured to be
// unhealthy about. A configured-but-failing namespace does record one, so an
// operator who asked for floor records can see they are not arriving — but it
// still never blocks the devices snapshot or the endpoints that matter.
func (f *Fetcher) refreshFloorplan(ctx context.Context, c *remoteconfig.Client) {
	if f.FloorplanNamespace == "" {
		return
	}
	var doc floorplanDocument
	if err := c.Get(ctx, f.FloorplanNamespace, &doc); err != nil {
		f.warn("remote config: floorplan namespace unavailable, keeping last-known",
			"namespace", f.FloorplanNamespace, "error", err)
		f.tracker.Record(f.FloorplanNamespace, err)
		return
	}
	floors, rooms := doc.Floors, doc.Rooms
	if floors == nil {
		floors = map[string]FloorConfig{}
	}
	if rooms == nil {
		rooms = map[string]RoomConfig{}
	}
	normaliseFloors(floors)
	normaliseRooms(rooms)
	// Swapped together: they come from one document, so a reader must never see
	// this fetch's rooms beside the previous fetch's floors.
	f.mu.Lock()
	f.floors, f.rooms = floors, rooms
	f.mu.Unlock()
	f.tracker.Record(f.FloorplanNamespace, nil)
}

// refreshDevices decodes the namespace with the shared schema, field by field:
// a value that does not fit costs that field, not the snapshot. Problems are
// logged, never refused.
func (f *Fetcher) refreshDevices(ctx context.Context, c *remoteconfig.Client) {
	ns := f.DevicesNamespace
	var raw json.RawMessage
	err := c.Get(ctx, ns, &raw)
	var decoded map[string]DeviceConfig
	var problems []devices.Problem
	if err == nil {
		decoded, problems, err = devices.Decode(raw)
	}
	if err != nil {
		f.warn("remote config: devices namespace unavailable, keeping last-known",
			"namespace", ns, "error", err)
		f.tracker.Record(ns, err)
		return
	}
	for _, p := range append(problems, devices.Check(decoded)...) {
		f.warn("remote config: devices problem", "namespace", ns, "device", p.ID,
			"field", p.Field, "code", p.Code, "problem", p.Message)
	}
	f.mu.Lock()
	f.devices = decoded
	f.mu.Unlock()
	f.tracker.Record(ns, nil)
}

func (f *Fetcher) warn(msg string, args ...any) {
	if f.Logger != nil {
		f.Logger.Warn(msg, args...)
	}
}
