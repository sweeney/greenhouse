package httpapi

import (
	"fmt"
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
// chart, honouring the optional devices=, rooms= and floors= CSV query filters.
//
// The candidate set is ALWAYS climate devices only (config.ReportsEnvironment):
// greenhouse charts climate, so a non-climate device that happens to share a
// room is never a candidate (class is applied before room and floor). The three
// filters compose as AND — a device must satisfy all of them to survive. With no
// filter, every climate device is returned (the prior behaviour).
//
// Validation writes a 400 (and returns ok=false) when:
//   - devices= names an id absent from the inventory, or one that exists but is
//     not a climate sensor;
//   - rooms= names a room with no climate sensor (which includes a room holding
//     only non-climate devices) — that room does not exist as far as the climate
//     API is concerned, so it is an error, not an empty series;
//   - floors= names a floor with no climate sensor, for the same reason.
//
// A valid set of filters whose intersection is empty is NOT an error: it yields
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

	// `locations=` was removed with the rest of the alias. Rejecting it rather than
	// ignoring it matters: an unrecognised parameter would silently widen the request
	// to every device, so a client asking for one room would get a chart of the whole
	// house with nothing to explain it. Its sibling group_by=location already 400s.
	if q.Has("locations") {
		writeError(w, http.StatusBadRequest,
			"'locations' was removed; use 'rooms' with floorplan room ids")
		return nil, false
	}

	rooms := splitCSV(q.Get("rooms"))

	// floors= is the coarse sibling of rooms=: a floor is the set of rooms whose
	// declared floor matches, so `floors=floor2` saves the caller enumerating the
	// floorplan just to chart a storey.
	floors := splitCSV(q.Get("floors"))

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

	// Validate requested rooms against rooms that hold a climate sensor.
	if len(rooms) > 0 {
		climateRooms := make(map[string]struct{})
		for _, d := range candidate {
			if p := d.Place(); p != "" {
				climateRooms[p] = struct{}{}
			}
		}
		for _, l := range rooms {
			if _, ok := climateRooms[l]; !ok {
				writeError(w, http.StatusBadRequest, "unknown room in 'rooms': "+l)
				return nil, false
			}
		}
	}

	// Validate requested floors against floors that hold a climate sensor. A
	// device whose namespace entry declares no floor has an UNKNOWN floor, not a
	// floor of its own, so it contributes nothing here and can never be selected
	// by floors= — see config.DeviceConfig.Floor.
	if len(floors) > 0 {
		climateFloors := make(map[string]struct{})
		for _, d := range candidate {
			if f := d.Floor; f != "" {
				climateFloors[f] = struct{}{}
			}
		}
		for _, f := range floors {
			if _, ok := climateFloors[f]; !ok {
				writeError(w, http.StatusBadRequest, "unknown floor in 'floors': "+f)
				return nil, false
			}
		}
	}

	idSet := toSet(ids)
	roomSet := toSet(rooms)
	floorSet := toSet(floors)

	out := make(map[string]config.DeviceConfig)
	for id, d := range candidate {
		if idSet != nil {
			if _, ok := idSet[id]; !ok {
				continue
			}
		}
		if roomSet != nil {
			if _, ok := roomSet[d.Place()]; !ok {
				continue
			}
		}
		if floorSet != nil {
			if _, ok := floorSet[d.Floor]; !ok {
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
	case climate.GroupByDevice, climate.GroupByRoom, climate.GroupByFloor:
		return true
	default:
		return false
	}
}

// resolveGroupFn parses and validates group_fn: how a group's MEMBERS are
// combined once fn= has collapsed each device's samples within a bucket. It
// writes a 400 (and returns ok=false) when:
//
//   - group_fn is not one of mean/min/max. `last` is called out by name because
//     it is the plausible-looking mistake: it is valid for fn=, but across
//     members it would mean "whichever sensor reported most recently", which is
//     not a spatial statistic;
//   - group_fn is given alongside a grouping that combines nothing
//     (group_by=device). That request is the identity case, and a caller who
//     wrote it believes an aggregation is happening that is not. Failing loudly
//     matches the house style and catches a real client bug.
//
// Omitting group_fn resolves to climate.DefaultGroupFn (mean), which is what
// greenhouse always did — so every request that predates this parameter is
// unchanged.
func (s *Server) resolveGroupFn(w http.ResponseWriter, r *http.Request, groupBy string) (string, bool) {
	q := r.URL.Query()
	if !q.Has("group_fn") {
		return climate.DefaultGroupFn, true
	}
	groupFn := q.Get("group_fn")
	if !climate.Groups(groupBy) {
		writeError(w, http.StatusBadRequest,
			"'group_fn' combines a group's members, but group_by="+groupBy+
				" gives every device its own series; drop 'group_fn' or group by "+
				climate.GroupByRoom+"/"+climate.GroupByFloor)
		return "", false
	}
	if groupFn == "last" {
		writeError(w, http.StatusBadRequest,
			"invalid 'group_fn': 'last' is valid for 'fn' but not across a group's "+
				"members, where it would mean 'whichever sensor reported most recently' "+
				"— not a spatial statistic (want one of "+strings.Join(climate.GroupFns(), ", ")+")")
		return "", false
	}
	if !climate.ValidGroupFn(groupFn) {
		writeError(w, http.StatusBadRequest,
			"invalid 'group_fn' (want one of "+strings.Join(climate.GroupFns(), ", ")+")")
		return "", false
	}
	return groupFn, true
}

// catalogEntry is one row in the /devices catalog.
type catalogEntry struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Room        string `json:"room"`
	// Floor is the floor the devices namespace declares for this device, passed
	// through as-is. It is empty when the namespace declares none, which the
	// catalog reports as unknown rather than guessing one from the room id.
	Floor string `json:"floor"`
	Class string `json:"class"`
	// EnvironmentFields mirrors the config key of the same name: the fields
	// this device actually writes to `device_environment`. Named to match, so
	// the catalog and the namespace describe the same thing with one word.
	EnvironmentFields []string `json:"environment_fields"`
}

// handleDevices serves GET /devices: the climate device catalog. It returns
// every device whose class reports environmental telemetry (see
// config.ReportsEnvironment — environmental_sensor and fire_alarm), each with
// its room, its declared floor, and an `environment_fields` hint of the fields
// it writes.
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
			Room:              dev.Place(),
			Floor:             dev.Floor,
			Class:             dev.Class,
			EnvironmentFields: fields,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"devices": out})
}

// handleSeries serves GET /series: a multi-series, columnar climate time-series
// for one field, grouped by device (default) or room (mean per room).
func (s *Server) handleSeries(w http.ResponseWriter, r *http.Request) {
	groupBy := r.URL.Query().Get("group_by")
	if groupBy == "" {
		groupBy = climate.GroupByDevice
	}
	if !validGroupBy(groupBy) {
		writeError(w, http.StatusBadRequest, "invalid 'group_by' (want one of device, room, floor)")
		return
	}
	groupFn, ok := s.resolveGroupFn(w, r, groupBy)
	if !ok {
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

	// A circular field (wind direction) cannot be combined across a group's
	// members: mean(350°, 10°) is 180° (South) though both readings say North.
	// fn= already refuses the linear aggregations for these fields; refusing here
	// closes the same hole on the grouping axis.
	//
	// Said up front rather than served as gaps: null means "no reading", so a
	// silently-gapped series is indistinguishable from an outage — the same
	// reasoning as handleDeviceSeries' unreportable-field 400. Narrowing the
	// request (rooms=, devices=) until each group holds one sensor, or
	// group_by=device, answers it honestly.
	if key, n, conflict := climate.CircularGroupConflict(field, groupBy, devices); conflict {
		writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"cannot group '%s' by %s: %s holds %d sensors and a 0–360° bearing has no arithmetic mean; use group_by=device",
			field, groupBy, key, n))
		return
	}

	win, iv, ok := s.resolveSeriesParams(w, r)
	if !ok {
		return
	}

	resp, err := s.buildSeries(r, win, iv, field, fn, groupBy, groupFn, devices)
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
	// This endpoint always groups by device — it promises exactly one series for
	// a named device — so group_fn is rejected here exactly as it is on
	// /series?group_by=device. It is NOT ignored the way devices=/rooms=/floors=
	// are: those SELECT, and the path segment has already selected, so repeating
	// them is redundancy. group_fn selects nothing; it asks for an aggregation
	// that cannot occur, which is a client bug held more strongly here than on
	// /series. The two endpoints must answer the same mistake the same way, or
	// moving between them silently turns a 400 into a no-op.
	if _, ok := s.resolveGroupFn(w, r, climate.GroupByDevice); !ok {
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
	resp, err := s.buildSeries(r, win, iv, field, fn, climate.GroupByDevice, climate.DefaultGroupFn, single)
	if err != nil {
		writeError(w, http.StatusBadGateway, "influx query failed: "+err.Error())
		return
	}
	writeSeriesShaped(w, shape, resp)
}

// buildSeries runs climate.BuildSeries.
func (s *Server) buildSeries(r *http.Request, win climate.Window, iv climate.Interval, field, fn, groupBy, groupFn string, devices map[string]config.DeviceConfig) (climate.SeriesResponse, error) {
	return climate.BuildSeries(r.Context(), s.Influx, s.Bucket, win, iv, field, fn, groupBy, groupFn,
		devices, s.groupLabels(groupBy), s.loc())
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

// floorEntry is one row in the /floors catalog: a floor id plus whatever the
// floorplan namespace declares about it.
type floorEntry struct {
	// ID is the value to pass back as floors= and the value devices carry in
	// their `floor` property.
	ID string `json:"id"`
	// Name is the floorplan's display label, empty when it declares none. The
	// catalog reports it empty rather than title-casing the id and hoping.
	Name string `json:"name"`
	// Order is the storey position, ascending from the lowest. Null when the
	// floorplan declares none — a pointer because 0 is a legitimate order (a
	// basement) and so cannot double as "undeclared".
	Order *int `json:"order"`
	// Elevation is metres above the site datum, null when undeclared. Pointer for
	// the same reason as Order: 0.0 is a real elevation.
	Elevation *float64 `json:"elevation"`
	// DeviceCount is how many climate devices declare this floor. Always at least
	// 1, since a floor with none is not listed.
	DeviceCount int `json:"device_count"`
}

// handleFloors serves GET /floors: the floor catalog, and the discoverable
// vocabulary behind `floors=` — the relationship /fields has to `field=`.
//
// WHICH floors are listed is the contract that matters. It is exactly the floors
// at least one CLIMATE device declares, which is exactly the set `floors=`
// accepts. The two are derived from the same predicate deliberately: if /floors
// advertised a floor that /series rejected with "unknown floor", a client filling
// a picker from this endpoint would build a broken control out of correct data.
// So a floorplan record naming a floor with no climate sensor is NOT listed — it
// exists in the building, but not as far as the climate API is concerned — and a
// floor devices declare but the floorplan has no record for IS listed, with its
// name and order null.
//
// That second case is why name and order are nullable rather than defaulted.
// Greenhouse passes the floorplan's answers through and reports UNKNOWN where it
// has none; it never derives a name from the id or a position from sort order,
// for the same reason it never derives a device's floor from its room id.
//
// Ordering: by declared order ascending, then by id, so floors with records come
// back in building order and undeclared ones sort last deterministically rather
// than wherever a map iteration left them.
func (s *Server) handleFloors(w http.ResponseWriter, _ *http.Request) {
	records := s.floors()

	// The key function comes from climate.GroupKeyFor, so this listing cannot
	// disagree with floors= or group_by=floor about which floor a device is on:
	// they are the same function, not two expressions that happen to match. An
	// empty key is UNKNOWN — never listed, never matched by floors=. See
	// config.DeviceConfig.Floor.
	counts := countDevicesByGroupKey(s.Config.Devices(), climate.GroupByFloor)

	out := make([]floorEntry, 0, len(counts))
	for id, n := range counts {
		e := floorEntry{ID: id, DeviceCount: n}
		if rec, ok := records[id]; ok {
			e.Name, e.Order, e.Elevation = rec.Name, rec.Order, rec.Elevation
		}
		out = append(out, e)
	}

	sort.Slice(out, func(i, j int) bool {
		return floorPrecedes(records, out[i].ID, out[j].ID)
	})

	writeJSON(w, http.StatusOK, map[string]any{"floors": out})
}

// roomEntry is one row in the /rooms catalog: a room id plus whatever the
// floorplan namespace declares about it.
type roomEntry struct {
	// ID is the value to pass back as rooms= and the value devices carry in
	// their `room` property. It is the key: NAMES ARE NOT UNIQUE, since two
	// rooms on different floors may share one.
	ID string `json:"id"`
	// Name is the floorplan's display label, empty when it declares none. The
	// catalog reports it empty rather than deriving one from the id.
	Name string `json:"name"`
	// Floor is the floor id the floorplan puts this room on, empty when it
	// declares none. Passed through from the room record — never read out of the
	// id's "<floor>.<slug>" shape, and never inferred from the floors this
	// room's devices declare.
	//
	// NOT guaranteed to appear in /floors or to be accepted by floors=. That
	// listing is built from the floors DEVICES declare; this relays what the ROOM
	// record declares, and greenhouse does not arbitrate between two upstream
	// declarations. They diverge whenever a device has a room but no declared
	// floor, which config.DeviceConfig.Floor explicitly allows. A client joining
	// /rooms to /floors must handle a miss.
	Floor string `json:"floor"`
	// Category is the room's purpose as the floorplan classifies it, e.g.
	// "kitchen", "circulation", "plant". Relayed RAW rather than reduced to a
	// flag: whether a plant room "counts" is a per-client policy question.
	Category string `json:"category"`
	// Area is the floor area in square metres, null when undeclared. A pointer
	// because absence differs from zero.
	Area *float64 `json:"area"`
	// DeviceCount is how many climate devices sit in this room. Always at least
	// 1, since a room with none is not listed.
	DeviceCount int `json:"device_count"`
}

// handleRooms serves GET /rooms: the room catalog, and the discoverable
// vocabulary behind `rooms=` — the room-shaped sibling of /floors.
//
// WHICH rooms are listed follows /floors exactly, and for the same reason: the
// rooms at least one CLIMATE device sits in, which is exactly the set `rooms=`
// accepts. A picker filled from this endpoint therefore cannot produce a 400. A
// floorplan record for a room with no climate sensor is NOT listed — it exists
// in the building, but not as far as the climate API is concerned — and a room
// devices declare that the floorplan has no record for IS listed, with its name,
// floor and category empty and area null.
//
// Every floorplan field is passed through UNCHANGED, including `category`. It is
// deliberately not reduced to a computed flag (is_living_space, charts_by_default
// or similar): whether a plant room "counts" is a per-client policy question, not
// a fact about the room. A floor-mean view excludes it, a "where is the heat
// going?" view wants it, and an equipment view wants only it — a boolean would
// bake the first caller's answer into the API and leave the other two working
// around it.
//
// Ordering: by floor as the floorplan orders it (declared storey order, then
// floor id), then by room id — so a client renders the list building-order
// top to bottom without re-sorting, and rooms whose floor is unknown sort last
// together rather than being scattered.
func (s *Server) handleRooms(w http.ResponseWriter, _ *http.Request) {
	records := s.rooms()

	// Same rule, same source: climate.GroupKeyFor is what rooms= and
	// group_by=room key on, so this listing agrees with them by construction. An
	// empty key is UNKNOWN — the device is charted by group_by=device and belongs
	// to no room here.
	counts := countDevicesByGroupKey(s.Config.Devices(), climate.GroupByRoom)

	floors := s.floors()
	out := make([]roomEntry, 0, len(counts))
	for id, n := range counts {
		e := roomEntry{ID: id, DeviceCount: n}
		if rec, ok := records[id]; ok {
			e.Name, e.Floor, e.Category, e.Area = rec.Name, rec.Floor, rec.Category, rec.Area
		}
		out = append(out, e)
	}

	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Floor != b.Floor {
			return floorPrecedes(floors, a.Floor, b.Floor)
		}
		return a.ID < b.ID
	})

	writeJSON(w, http.StatusOK, map[string]any{"rooms": out})
}

// countDevicesByGroupKey counts the CLIMATE devices in each group under groupBy,
// keyed by climate.GroupKeyFor — the single definition of "which devices share a
// series". Devices whose key is empty have UNKNOWN membership and are counted
// nowhere, so a catalog never lists a group the corresponding filter rejects.
//
// Deliberately only the counting: /floors and /rooms differ in entry type and in
// what they enrich from, so sharing more than this would need a type parameter
// and a callback to save a handful of lines. The key function is the part that
// must not drift.
func countDevicesByGroupKey(devices map[string]config.DeviceConfig, groupBy string) map[string]int {
	keyOf := climate.GroupKeyFor(groupBy)
	counts := map[string]int{}
	for _, dev := range devices {
		if !dev.ReportsEnvironment() {
			continue
		}
		if k := keyOf(dev); k != "" {
			counts[k]++
		}
	}
	return counts
}

// floorPrecedes reports whether floor a sorts before floor b: declared storey
// order ascending, undeclared last, ties broken by id. Both catalogs sort
// through it, so they cannot order the same floors differently.
//
// A room whose floor is UNKNOWN ("") sorts after every known floor rather than
// first, so unplaced rooms gather at the end instead of leading the list.
//
// A floor named by a room record but absent from floors — possible, because
// /floors lists the floors DEVICES declare while a room relays what its own
// record declares (see roomEntry.Floor) — lands on the zero FloorConfig and so
// has no order. That is deliberate, not incidental: such a floor is genuinely
// unordered as far as greenhouse knows, so it sorts with the other order-less
// floors rather than being given a position nobody published.
func floorPrecedes(floors map[string]config.FloorConfig, a, b string) bool {
	if a == "" || b == "" {
		return a != "" // a known floor precedes an unknown one
	}
	ao, bo := floors[a].Order, floors[b].Order
	switch {
	case ao != nil && bo != nil && *ao != *bo:
		return *ao < *bo
	case ao != nil && bo == nil:
		return true
	case ao == nil && bo != nil:
		return false
	}
	return a < b
}

// handleFields serves GET /fields: the field registry (name, unit, default fn)
// so consumers can build field pickers.
func (s *Server) handleFields(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"fields": climate.Fields(),
	})
}
