package httpapi

import (
	"net/http"
	"sort"
	"time"

	"github.com/sweeney/greenhouse/internal/climate"
	"github.com/sweeney/greenhouse/internal/config"
	"github.com/sweeney/greenhouse/internal/influx"
)

// environmentalClass is the device class greenhouse charts.
const environmentalClass = "environmental_sensor"

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
	if _, known := climate.FieldFor(field); !known {
		writeError(w, http.StatusBadRequest, "unknown 'field': "+field)
		return "", "", false
	}
	fn = q.Get("fn")
	if fn == "" {
		fn = climate.DefaultFn
	}
	if !climate.ValidFn(fn) {
		writeError(w, http.StatusBadRequest, "invalid 'fn' (want one of mean, min, max, last)")
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
	ID          string   `json:"id"`
	DisplayName string   `json:"display_name"`
	Location    string   `json:"location"`
	Class       string   `json:"class"`
	Fields      []string `json:"fields"`
}

// handleDevices serves GET /devices: the climate device catalog. It returns the
// environmental_sensor devices from the statehouse_devices snapshot, each with a
// `fields` hint of the environmental fields it reports. The hint comes from the
// device config's explicit `fields` list when present; otherwise it falls back
// to the full field registry (greenhouse cannot know per-device coverage without
// querying Influx, and the registry is a safe superset for building a picker).
// Sorted by id for stable output.
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
		if dev.Class != environmentalClass {
			continue
		}
		fields := dev.Fields
		if len(fields) == 0 {
			fields = climate.FieldNames()
		}
		out = append(out, catalogEntry{
			ID:          id,
			DisplayName: dev.DisplayName,
			Location:    dev.Location,
			Class:       dev.Class,
			Fields:      fields,
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

	win, iv, ok := s.resolveSeriesParams(w, r)
	if !ok {
		return
	}

	resp, err := s.buildSeries(r, win, iv, field, fn, groupBy, s.Config.Devices())
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
	if dev.Class != environmentalClass {
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
	if dev.Class != environmentalClass {
		writeError(w, http.StatusBadRequest, "device is not a climate sensor")
		return
	}

	flux := influx.BuildLatestFlux(s.Bucket, id)
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
			Value: roundTo(row.Value, valueDP),
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
