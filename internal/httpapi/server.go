// Package httpapi hosts greenhouse's read-side JSON HTTP API.
//
// It mirrors statehouse/countinghouse server conventions: handlers are methods
// on Server, routes are centralised in newMux (so tests exercise exactly the
// running routes), and Start(ctx) runs an http.Server with a 5s graceful
// shutdown. Two paths are public (/healthz, /openapi.json); data routes are
// wrapped by authMiddleware, which accepts both user and service tokens.
package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime"
	"time"

	"github.com/sweeney/greenhouse/internal/climate"
	"github.com/sweeney/greenhouse/internal/config"
	"github.com/sweeney/greenhouse/internal/influx"
	"github.com/sweeney/greenhouse/internal/testutil"
	"github.com/sweeney/identity/common/auth"
	"github.com/sweeney/identity/common/spec"
)

// ConfigProvider supplies the current remote-config snapshot the data handlers
// need. The real implementation is the Fetcher (which refreshes the site's
// devices namespace on SIGHUP); tests inject a fake. The method returns the
// latest snapshot and must be safe for concurrent use.
type ConfigProvider interface {
	// Devices returns the current devices-namespace snapshot keyed by
	// device_id. Used to find climate devices and group series by location.
	Devices() map[string]config.DeviceConfig
}

// FloorplanProvider supplies the floorplan snapshot behind /floors and /rooms,
// and the display names grouped series are labelled with. The Fetcher satisfies
// it; tests inject a fake.
//
// It is OPTIONAL — Server.Floorplan may be nil, and then both catalogs still
// list everything that holds a climate sensor, with names, order and category
// reported as unknown, and grouped series stay labelled by id. Floorplan detail
// is presentation, so a missing one must never stop a climate service serving
// climate.
//
// One interface rather than two because both collections come from one document
// in one namespace: splitting them would let a caller hold half a floorplan and
// suggest the halves can be configured independently, which they cannot.
type FloorplanProvider interface {
	// Floors returns the current floor records keyed by floor id.
	Floors() map[string]config.FloorConfig
	// Rooms returns the current room records keyed by floorplan room id.
	Rooms() map[string]config.RoomConfig
}

// ConfigStatus surfaces the remote-config fetcher's per-namespace status for
// /healthz. The Fetcher satisfies it; tests inject a fake. May be nil (then
// /healthz omits remote_config).
type ConfigStatus interface {
	Statuses() map[string]config.NamespaceStatus
}

// Server hosts the JSON HTTP API.
type Server struct {
	// Listen is the bind address, e.g. ":8082".
	Listen string

	// Influx is the read-side query client. Used by /healthz for a reachability
	// ping; data handlers query through it too. May be nil in tests that don't
	// exercise Influx.
	Influx influx.Querier

	// Logger receives structured output. May be nil.
	Logger *slog.Logger

	// IdentityURL is the base URL of the identity service (e.g.
	// "https://id.swee.net"). When set, data routes require a valid Bearer JWT
	// (user OR service token). When empty, auth is disabled (local dev/tests).
	IdentityURL string

	// PublicURL is the externally-reachable base URL of this server. When set it
	// is substituted into the OpenAPI spec's servers list; empty leaves the
	// placeholder as-is.
	PublicURL string

	// Version is the build commit set via -ldflags; empty when running outside a
	// tagged deploy.
	Version string

	// SiteID and DevicesNamespace are the resolved site config, reported on
	// /healthz so an operator can see which property this instance believes it
	// serves rather than inferring it from whether the charts look plausible.
	// Both empty on an instance predating the per-site split, which is not a fault.
	SiteID           string
	DevicesNamespace string

	// FloorplanNamespace is the optional floorplan namespace, reported on
	// /healthz so an operator can tell "not configured" from "configured but not
	// yet fetched" without reading the host's config file. Empty on the instances
	// that do not set one, which is not a fault.
	FloorplanNamespace string

	// Bucket is the Influx bucket the data handlers query (e.g. "statehouse").
	// main.go sets it from config.
	Bucket string

	// Clock sources the current time for window resolution. Logic must never
	// call time.Now() directly. Defaults to testutil.RealClock{} when nil.
	Clock testutil.Clock

	// Loc is the timezone calendar window boundaries are computed in. Defaults
	// to time.UTC when nil; main.go sets Europe/London from config.
	Loc *time.Location

	// Config supplies the current device snapshot. The real impl is the
	// Fetcher; tests inject a fake. May be nil only for the public-route tests
	// (data handlers require it).
	Config ConfigProvider

	// Floorplan supplies floor and room records for /floors, /rooms and the
	// labels on grouped series. The real impl is the Fetcher; tests inject a
	// fake. May be nil (and is, whenever no floorplan namespace is configured) —
	// the catalogs then report names, order and category as unknown, and grouped
	// series stay labelled by id, rather than failing.
	Floorplan FloorplanProvider

	// RemoteConfig surfaces per-namespace remote-config fetch status on
	// /healthz. The real impl is the Fetcher (which satisfies ConfigStatus);
	// tests may inject a fake or leave it nil (then /healthz omits the field).
	RemoteConfig ConfigStatus

	started time.Time

	srv           *http.Server
	verifier      *auth.JWKSVerifier
	specConverter *spec.Converter
}

// clock returns the configured Clock, defaulting to a real clock.
func (s *Server) clock() testutil.Clock {
	if s.Clock != nil {
		return s.Clock
	}
	return testutil.RealClock{}
}

// floors returns the current floor records, or nil when no floorplan provider is
// configured. A nil map reads as empty, so /floors degrades to "every floor is
// unknown" rather than panicking on an instance with no floorplan namespace.
func (s *Server) floors() map[string]config.FloorConfig {
	if s.Floorplan == nil {
		return nil
	}
	return s.Floorplan.Floors()
}

// rooms returns the current room records, nil-safe for the same reason as floors.
func (s *Server) rooms() map[string]config.RoomConfig {
	if s.Floorplan == nil {
		return nil
	}
	return s.Floorplan.Rooms()
}

// groupLabels maps each group key to the floorplan's display name for it, for
// whichever grouping the request asked for. Keys the floorplan does not name are
// absent, and the assembly step then labels those series by id.
//
// Built per request from the live snapshot rather than cached, so a SIGHUP
// reload is reflected immediately. Labels are cosmetic, so no attempt is made to
// pin them to the same instant as the device snapshot: the worst a reload racing
// a request can produce is a series labelled by its id for one response.
func (s *Server) groupLabels(groupBy string) map[string]string {
	out := map[string]string{}
	switch groupBy {
	case climate.GroupByRoom:
		for id, r := range s.rooms() {
			if r.Name != "" {
				out[id] = r.Name
			}
		}
	case climate.GroupByFloor:
		for id, f := range s.floors() {
			if f.Name != "" {
				out[id] = f.Name
			}
		}
	}
	return out
}

// loc returns the configured timezone, defaulting to UTC.
func (s *Server) loc() *time.Location {
	if s.Loc != nil {
		return s.Loc
	}
	return time.UTC
}

// New returns a configured Server. Optional fields (IdentityURL, PublicURL,
// Version, Logger) are set by the caller after construction, mirroring
// statehouse/countinghouse.
func New(listen string, querier influx.Querier, logger *slog.Logger) *Server {
	return &Server{
		Listen:  listen,
		Influx:  querier,
		Logger:  logger,
		started: time.Now().UTC(),
	}
}

// newMux builds and returns the ServeMux used by both Start and tests.
// Centralising route registration here means tests always exercise the same
// routes as the running server.
func newMux(s *Server) *http.ServeMux {
	s.specConverter = buildSpecConverter(s.PublicURL)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/openapi.json", s.handleOpenAPIJSON)

	// auth wraps every data route: a valid Bearer JWT (user OR service token)
	// is required when IdentityURL is set, and it is a no-op otherwise.
	authmw := s.authMiddleware()
	mux.Handle("GET /devices", authmw(http.HandlerFunc(s.handleDevices)))
	mux.Handle("GET /devices/{id}/series", authmw(http.HandlerFunc(s.handleDeviceSeries)))
	mux.Handle("GET /devices/{id}/latest", authmw(http.HandlerFunc(s.handleDeviceLatest)))
	mux.Handle("GET /series", authmw(http.HandlerFunc(s.handleSeries)))
	mux.Handle("GET /floors", authmw(http.HandlerFunc(s.handleFloors)))
	mux.Handle("GET /rooms", authmw(http.HandlerFunc(s.handleRooms)))
	mux.Handle("GET /fields", authmw(http.HandlerFunc(s.handleFields)))
	return mux
}

// handler returns the fully-wrapped HTTP handler the server serves: the route
// mux behind the CORS middleware (so browser consumers can call the API).
func (s *Server) handler() http.Handler {
	return corsMiddleware(newMux(s))
}

// Start runs the HTTP server until the context is cancelled.
func (s *Server) Start(ctx context.Context) error {
	if s.started.IsZero() {
		s.started = time.Now().UTC()
	}
	s.srv = &http.Server{
		Addr:              s.Listen,
		Handler:           s.handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		err := s.srv.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// siteHealth is the resolved site config as reported by /healthz: which property this
// instance serves, and where it reads that property's devices from. Previously
// answerable only by reading the host's config file — or by noticing the charts were
// of the wrong building.
type siteHealth struct {
	ID               string `json:"id,omitempty"`
	DevicesNamespace string `json:"devices_namespace,omitempty"`
	// FloorplanNamespace is reported for the same reason, and answers a question
	// the remote_config block cannot: that block distinguishes "configured and
	// failing" from "configured and fine" only AFTER a fetch attempt, so an
	// operator seeing blank floor names cannot otherwise tell "no floorplan
	// namespace configured" from "configured, first fetch hasn't landed" without
	// reading the host's config file. omitempty keeps it invisible on the
	// instances that do not set it.
	FloorplanNamespace string `json:"floorplan_namespace,omitempty"`
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	type health struct {
		Status          string                            `json:"status"`
		Version         string                            `json:"version,omitempty"`
		StartedAt       time.Time                         `json:"started_at"`
		StartedAgo      int                               `json:"started_ago"`
		Goroutines      int                               `json:"goroutines"`
		Auth            string                            `json:"auth"`
		InfluxReachable bool                              `json:"influx_reachable"`
		Site            *siteHealth                       `json:"site,omitempty"`
		RemoteConfig    map[string]config.NamespaceStatus `json:"remote_config,omitempty"`
	}
	// auth posture so monitoring can alert on an unauthenticated data API.
	authStatus := "enabled"
	if s.IdentityURL == "" {
		authStatus = "disabled"
	}
	var site *siteHealth
	if s.SiteID != "" || s.DevicesNamespace != "" || s.FloorplanNamespace != "" {
		site = &siteHealth{
			ID:                 s.SiteID,
			DevicesNamespace:   s.DevicesNamespace,
			FloorplanNamespace: s.FloorplanNamespace,
		}
	}
	h := health{
		Site:       site,
		Status:     "ok",
		Version:    s.Version,
		StartedAt:  s.started,
		StartedAgo: int((time.Since(s.started) + 500*time.Millisecond) / time.Second),
		Goroutines: runtime.NumGoroutine(),
		Auth:       authStatus,
	}
	if s.Influx != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		h.InfluxReachable = s.Influx.Ping(ctx)
	}
	if s.RemoteConfig != nil {
		h.RemoteConfig = s.RemoteConfig.Statuses()
	}
	writeJSON(w, http.StatusOK, h)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
