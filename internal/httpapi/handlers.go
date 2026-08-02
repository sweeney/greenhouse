package httpapi

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/sweeney/greenhouse/internal/climate"
	"github.com/sweeney/greenhouse/internal/config"
	"github.com/sweeney/greenhouse/internal/influx"
)

// resolveWindowParams parses the window/from/to query params and resolves them
// to a concrete Window using the injected clock + location. It returns a 400
// (written to w) and ok=false on any bad/missing param or unknown window.
//
// window defaults to "today" when absent. For window=custom, from and to are
// required RFC3339 timestamps.
func (s *Server) resolveWindowParams(w http.ResponseWriter, r *http.Request) (climate.Window, bool) {
	q := r.URL.Query()
	spec := q.Get("window")
	if spec == "" {
		spec = climate.WindowToday
	}

	var from, to time.Time
	if v := q.Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid 'from' (want RFC3339): "+err.Error())
			return climate.Window{}, false
		}
		from = t
	}
	if v := q.Get("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid 'to' (want RFC3339): "+err.Error())
			return climate.Window{}, false
		}
		to = t
	}

	// from/to are meaningful ONLY with window=custom. Reject the contradictory
	// combination so a mistake surfaces as a 400 rather than silently-wrong data:
	// without this guard a today/week/month request carrying from/to would parse
	// them and then discard them, returning an unrelated range. This is the
	// inverse of the custom-side strictness (custom requires from/to), making the
	// contract symmetric: from/to <=> custom.
	if spec != climate.WindowCustom && (!from.IsZero() || !to.IsZero()) {
		writeError(w, http.StatusBadRequest, "'from'/'to' are only valid with window=custom")
		return climate.Window{}, false
	}

	win, err := climate.ResolveWindow(s.clock().Now(), s.loc(), spec, from, to)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return climate.Window{}, false
	}
	return win, true
}

// resolveSeriesParams resolves the window and interval shared by the series
// handlers. It writes a 400 (and returns ok=false) on a bad window, a bad
// interval, or an interval whose bucket count exceeds the cap.
func (s *Server) resolveSeriesParams(w http.ResponseWriter, r *http.Request) (climate.Window, climate.Interval, bool) {
	win, ok := s.resolveWindowParams(w, r)
	if !ok {
		return climate.Window{}, climate.Interval{}, false
	}
	iv, err := climate.ResolveInterval(win, r.URL.Query().Get("interval"), s.loc())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return climate.Window{}, climate.Interval{}, false
	}
	return win, iv, true
}

// resolveFieldFn parses and validates the field and fn query params, defaulting
// field to temperature_c and fn to mean. It writes a 400 (and returns ok=false)
// on an unknown field or a disallowed fn.
func (s *Server) resolveFieldFn(w http.ResponseWriter, r *http.Request) (field, fn string, ok bool) {
	q := r.URL.Query()
	field = q.Get("field")
	if field == "" {
		field = climate.DefaultField
	}
	f, known := climate.FieldFor(field)
	if !known {
		writeError(w, http.StatusBadRequest, "unknown 'field': "+field)
		return "", "", false
	}
	fn = q.Get("fn")
	if fn == "" {
		// Default to the field's own aggregation, not the global mean: circular
		// fields (wind_dir_deg) default to last, which mean would 400 on.
		fn = f.DefaultFn
	}
	if !climate.ValidFnForField(f, fn) {
		if f.Circular {
			writeError(w, http.StatusBadRequest,
				"invalid 'fn' for circular field "+field+": only 'last' is valid (arithmetic mean/min/max are wrong for a 0–360° bearing)")
		} else {
			writeError(w, http.StatusBadRequest, "invalid 'fn' (want one of mean, min, max, last)")
		}
		return "", "", false
	}
	return field, fn, true
}

// lookupDevice returns the device config for id, writing a 404 and returning
// ok=false when the id is unknown.
func (s *Server) lookupDevice(w http.ResponseWriter, id string) (config.DeviceConfig, bool) {
	dev, ok := s.Config.Devices()[id]
	if !ok {
		writeError(w, http.StatusNotFound, "unknown device: "+id)
		return config.DeviceConfig{}, false
	}
	return dev, true
}

// resolveDeviceFilter builds the climate device set a /series request should
// chart, honouring the optional devices= and locations= CSV query filters.
//
// The candidate set is ALWAYS climate devices only (config.ReportsEnvironment):
// greenhouse charts climate, so a non-climate device that happens to share a
// location is never a candidate (class is applied before location). The two
// filters compose as AND — a device must satisfy both to survive. With neither
// filter, every climate device is returned (the prior behaviour).
//
// Validation writes a 400 (and returns ok=false) when:
//   - devices= names an id absent from the inventory, or one that exists but is
//     not a climate sensor;
//   - locations= names a location with no climate sensor (which includes a
//     location holding only non-climate devices) — that location does not exist
//     as far as the climate API is concerned, so it is an error, not an empty
//     series.
//
// A valid pair of filters whose intersection is empty is NOT an error: it yields
// an empty series list (200), consistent with a window that simply has no data.
func (s *Server) resolveDeviceFilter(w http.ResponseWriter, r *http.Request) (map[string]config.DeviceConfig, bool) {
	all := s.Config.Devices()

	// Candidate set: climate devices only.
	candidate := make(map[string]config.DeviceConfig)
	for id, d := range all {
		if d.ReportsEnvironment() {
			candidate[id] = d
		}
	}

	q := r.URL.Query()
	ids := splitCSV(q.Get("devices"))
	locs := splitCSV(q.Get("locations"))

	// Validate requested ids against the inventory and the climate class.
	for _, id := range ids {
		d, exists := all[id]
		if !exists {
			writeError(w, http.StatusBadRequest, "unknown device in 'devices': "+id)
			return nil, false
		}
		if !d.ReportsEnvironment() {
			writeError(w, http.StatusBadRequest, "device is not a climate sensor: "+id)
			return nil, false
		}
	}

	// Validate requested locations against locations that hold a climate sensor.
	if len(locs) > 0 {
		climateLocs := make(map[string]struct{})
		for _, d := range candidate {
			if d.Location != "" {
				climateLocs[d.Location] = struct{}{}
			}
		}
		for _, l := range locs {
			if _, ok := climateLocs[l]; !ok {
				writeError(w, http.StatusBadRequest, "unknown location in 'locations': "+l)
				return nil, false
			}
		}
	}

	idSet := toSet(ids)
	locSet := toSet(locs)

	out := make(map[string]config.DeviceConfig)
	for id, d := range candidate {
		if idSet != nil {
			if _, ok := idSet[id]; !ok {
				continue
			}
		}
		if locSet != nil {
			if _, ok := locSet[d.Location]; !ok {
				continue
			}
		}
		out[id] = d
	}
	return out, true
}

// filterDevicesReportingField drops devices that cannot report field, per their
// declared environment_fields. Devices that declare nothing are kept (coverage
// unknown — see config.MayReportField), so this only ever narrows on a positive
// declaration and can never hide data greenhouse was not told about.
func filterDevicesReportingField(devices map[string]config.DeviceConfig, field string) map[string]config.DeviceConfig {
	out := make(map[string]config.DeviceConfig, len(devices))
	for id, d := range devices {
		if d.MayReportField(field) {
			out[id] = d
		}
	}
	return out
}

// splitCSV splits a comma-separated query value into trimmed, non-empty parts.
// An empty input yields nil (no filter requested).
func splitCSV(v string) []string {
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// toSet turns a slice into a membership set, or nil for an empty slice (so
// callers can treat nil as "no filter").
func toSet(items []string) map[string]struct{} {
	if len(items) == 0 {
		return nil
	}
	m := make(map[string]struct{}, len(items))
	for _, it := range items {
		m[it] = struct{}{}
	}
	return m
}

// validGroupBy reports whether g is an accepted group_by mode.
func validGroupBy(g string) bool {
	switch g {
	case climate.GroupByDevice, climate.GroupByLocation:
		return true
	default:
		return false
	}
}

// catalogEntry is one row in the /devices catalog.
type catalogEntry struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Location    string `json:"location"`
	Class       string `json:"class"`
	// EnvironmentFields mirrors the config key of the same name: the fields
	// this device actually writes to `device_environment`. Named to match, so
	// the catalog and the namespace describe the same thing with one word.
	EnvironmentFields []string `json:"environment_fields"`
}

// handleDevices serves GET /devices: the climate device catalog. It returns
// every device whose class reports environmental telemetry (see
// config.ReportsEnvironment — environmental_sensor and fire_alarm), each with
// an `environment_fields` hint of the fields it writes.
//
// The hint comes from the device config's explicit environment_fields list when
// present; otherwise it falls back to the full field registry. The fallback
// over-advertises — greenhouse cannot know per-device coverage without querying
// Influx — so populating environment_fields in the namespace is what makes this
// catalog honest. Sorted by id for stable output.
func (s *Server) handleDevices(w http.ResponseWriter, _ *http.Request) {
	devices := s.Config.Devices()

	ids := make([]string, 0, len(devices))
	for id := range devices {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	out := make([]catalogEntry, 0, len(ids))
	for _, id := range ids {
		dev := devices[id]
		if !dev.ReportsEnvironment() {
			continue
		}
		fields := dev.EnvironmentFields
		if len(fields) == 0 {
			fields = climate.FieldNames()
		}
		out = append(out, catalogEntry{
			ID:                id,
			DisplayName:       dev.DisplayName,
			Location:          dev.Location,
			Class:             dev.Class,
			EnvironmentFields: fields,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"devices": out})
}

// handleSeries serves GET /series: a multi-series, columnar climate time-series
// for one field, grouped by device (default) or location (mean per room).
func (s *Server) handleSeries(w http.ResponseWriter, r *http.Request) {
	groupBy := r.URL.Query().Get("group_by")
	if groupBy == "" {
		groupBy = climate.GroupByDevice
	}
	if !validGroupBy(groupBy) {
		writeError(w, http.StatusBadRequest, "invalid 'group_by' (want one of device, location)")
		return
	}
	shape := r.URL.Query().Get("shape")
	if !climate.ValidShape(shape) {
		writeError(w, http.StatusBadRequest, "invalid 'shape' (want columns or rows)")
		return
	}
	field, fn, ok := s.resolveFieldFn(w, r)
	if !ok {
		return
	}
	devices, ok := s.resolveDeviceFilter(w, r)
	if !ok {
		return
	}
	// Drop devices that cannot report this field. /series promises a SET of
	// series, so devices with no possible data are irrelevant rather than an
	// error: without this, field=pressure_hpa returns one real line and nine
	// all-null ones, indistinguishable from nine offline sensors. An empty
	// result is a valid 200, consistent with a filter intersection that matches
	// nothing. (The single-device endpoint promises exactly one series and so
	// 400s instead — see handleDeviceSeries.)
	devices = filterDevicesReportingField(devices, field)

	win, iv, ok := s.resolveSeriesParams(w, r)
	if !ok {
		return
	}

	resp, err := s.buildSeries(r, win, iv, field, fn, groupBy, devices)
	if err != nil {
		writeError(w, http.StatusBadGateway, "influx query failed: "+err.Error())
		return
	}
	writeSeriesShaped(w, shape, resp)
}

// handleDeviceSeries serves GET /devices/{id}/series: the single-device
// convenience form. It returns the same SeriesResponse shape as /series (so
// consumers can share rendering code) carrying exactly one series for the
// requested device.
func (s *Server) handleDeviceSeries(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	dev, ok := s.lookupDevice(w, id)
	if !ok {
		return
	}
	if !dev.ReportsEnvironment() {
		writeError(w, http.StatusBadRequest, "device is not a climate sensor")
		return
	}
	shape := r.URL.Query().Get("shape")
	if !climate.ValidShape(shape) {
		writeError(w, http.StatusBadRequest, "invalid 'shape' (want columns or rows)")
		return
	}
	field, fn, ok := s.resolveFieldFn(w, r)
	if !ok {
		return
	}
	// This endpoint promises exactly one series for a named device, so a field
	// that device cannot report is an impossible request, not an empty one.
	// Answering 200-with-nulls would be indistinguishable from a sensor outage
	// (null means "no reading"), so say so plainly instead.
	if !dev.MayReportField(field) {
		writeError(w, http.StatusBadRequest,
			"device "+id+" does not report '"+field+"' (reports: "+strings.Join(dev.EnvironmentFields, ", ")+")")
		return
	}

	win, iv, ok := s.resolveSeriesParams(w, r)
	if !ok {
		return
	}

	single := map[string]config.DeviceConfig{id: dev}
	resp, err := s.buildSeries(r, win, iv, field, fn, climate.GroupByDevice, single)
	if err != nil {
		writeError(w, http.StatusBadGateway, "influx query failed: "+err.Error())
		return
	}
	writeSeriesShaped(w, shape, resp)
}

// buildSeries runs climate.BuildSeries.
func (s *Server) buildSeries(r *http.Request, win climate.Window, iv climate.Interval, field, fn, groupBy string, devices map[string]config.DeviceConfig) (climate.SeriesResponse, error) {
	return climate.BuildSeries(r.Context(), s.Influx, s.Bucket, win, iv, field, fn, groupBy, devices, s.loc())
}

// writeSeriesShaped writes a series response in the requested shape: the columnar
// SeriesResponse (default) or, for shape=rows, the row-oriented RowsResponse.
func writeSeriesShaped(w http.ResponseWriter, shape string, resp climate.SeriesResponse) {
	if shape == climate.ShapeRows {
		writeJSON(w, http.StatusOK, resp.Rows())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// latestReading is one field's most-recent value for a device.
type latestReading struct {
	Field string    `json:"field"`
	Unit  string    `json:"unit"`
	Value float64   `json:"value"`
	Time  time.Time `json:"time"`
}

// handleDeviceLatest serves GET /devices/{id}/latest: the device's most recent
// reading across all the fields it reports, for dashboards. Only registered
// fields are surfaced (unknown _field keys are skipped). Sorted by field name.
func (s *Server) handleDeviceLatest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	dev, ok := s.lookupDevice(w, id)
	if !ok {
		return
	}
	if !dev.ReportsEnvironment() {
		writeError(w, http.StatusBadRequest, "device is not a climate sensor")
		return
	}

	flux := influx.BuildLatestFlux(s.Bucket, id, influx.DefaultLatestLookback)
	rows, err := s.Influx.Query(r.Context(), flux)
	if err != nil {
		writeError(w, http.StatusBadGateway, "influx query failed: "+err.Error())
		return
	}

	readings := make([]latestReading, 0, len(rows))
	for _, row := range rows {
		if !row.HasValue {
			continue
		}
		f, known := climate.FieldFor(row.Field)
		if !known {
			continue
		}
		readings = append(readings, latestReading{
			Field: row.Field,
			Unit:  f.Unit,
			Value: climate.RoundValue(row.Value),
			Time:  row.Time,
		})
	}
	sort.Slice(readings, func(i, j int) bool { return readings[i].Field < readings[j].Field })

	writeJSON(w, http.StatusOK, map[string]any{
		"device_id": id,
		"readings":  readings,
	})
}

// handleFields serves GET /fields: the field registry (name, unit, default fn)
// so consumers can build field pickers.
func (s *Server) handleFields(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"fields": climate.Fields(),
	})
}
