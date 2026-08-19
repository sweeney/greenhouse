package httpapi

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sweeney/greenhouse/internal/influx"
)

// End-to-end cover for /floors: a real listener, the real router, real CORS and
// auth middleware, a signed ES256 service token, and a fake Influx underneath.
//
// It exists because the endpoint's whole value is a CONTRACT BETWEEN ENDPOINTS —
// a floor read off /floors must be a floor /series accepts, and the ordering must
// be usable without a client re-deriving anything. A unit test can pin each half;
// only the real stack shows a consumer the same answer twice.

func floorsIntegrationServer(t *testing.T) (base, token string, q *influx.FakeQuerier) {
	t.Helper()

	s, q := dataSetup(t)
	s.Config = fakeConfig{devices: catalogFloorDevices()}
	s.FloorRecords = fakeFloors{floors: catalogFloorRecords()}
	s.PublicURL = "https://greenhouse.swee.net"

	priv := genTestKey(t)
	const kid = "floorskey"
	id := fakeJWKSServer(t, &priv.PublicKey, kid)
	s.IdentityURL = id.URL

	q.QueryFunc = func(string) ([]influx.Row, error) {
		var rows []influx.Row
		for _, dev := range []string{"sensor_e", "sensor_f", "sensor_g", "alarm_a", "sensor_x"} {
			rows = append(rows, bucketRows(t, s, dev, "today", "1h", 20)...)
		}
		return rows, nil
	}

	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)

	return srv.URL, serviceToken(t, priv, kid, id.URL, time.Now().Add(time.Hour)), q
}

type floorsBody struct {
	Floors []floorRow `json:"floors"`
}

// The journey the endpoint exists for, and the one the demo page was hardcoding:
// fetch the floors, render them in building order with their real labels, then
// chart each one — without the client inventing an order or a name.
func TestIntegration_FloorPickerNeedsNoClientDerivation(t *testing.T) {
	base, token, _ := floorsIntegrationServer(t)

	// Not public: the catalog needs a token like every other data route.
	get(t, base, "", "/floors", http.StatusUnauthorized, nil)

	var body floorsBody
	get(t, base, token, "/floors", http.StatusOK, &body)

	if len(body.Floors) == 0 {
		t.Fatal("no floors listed")
	}

	// Building order arrives ready to render: the client sorts nothing.
	var lastOrder int
	var seenUnordered bool
	for i, f := range body.Floors {
		if f.Order == nil {
			seenUnordered = true
			continue
		}
		if seenUnordered {
			t.Errorf("position %d has a declared order after an undeclared one; "+
				"unknowns must sort last", i)
		}
		if i > 0 && *f.Order < lastOrder {
			t.Errorf("position %d order %d follows %d — not ascending", i, *f.Order, lastOrder)
		}
		lastOrder = *f.Order
	}

	// Labels arrive too, so nothing title-cases an id.
	byID := map[string]floorRow{}
	for _, f := range body.Floors {
		byID[f.ID] = f
	}
	if got := byID["floor1"].Name; got != "Lower Floor" {
		t.Errorf("floor1 name = %q, want the floorplan's own label", got)
	}

	// And every listed floor charts, which is the point of publishing the id.
	for _, f := range body.Floors {
		t.Run("chart "+f.ID, func(t *testing.T) {
			var resp struct {
				Series []struct {
					Key string `json:"key"`
				} `json:"series"`
			}
			get(t, base, token,
				"/series?window=today&interval=1h&floors="+f.ID, http.StatusOK, &resp)
			if len(resp.Series) == 0 {
				t.Errorf("floors=%s charted nothing", f.ID)
			}
		})
	}
}

// The endpoints must agree in BOTH directions: everything /floors lists is
// chartable, and every floor /devices reports is listed. A gap either way would
// mean a client discovering floors from one endpoint and being refused by another.
func TestIntegration_FloorsAgreesWithTheDeviceCatalog(t *testing.T) {
	base, token, _ := floorsIntegrationServer(t)

	var catalog struct {
		Devices []struct {
			ID    string `json:"id"`
			Floor string `json:"floor"`
		} `json:"devices"`
	}
	get(t, base, token, "/devices", http.StatusOK, &catalog)

	fromDevices := map[string]int{}
	for _, d := range catalog.Devices {
		if d.Floor != "" {
			fromDevices[d.Floor]++
		}
	}

	var body floorsBody
	get(t, base, token, "/floors", http.StatusOK, &body)

	fromFloors := map[string]int{}
	for _, f := range body.Floors {
		fromFloors[f.ID] = f.DeviceCount
	}

	if len(fromDevices) != len(fromFloors) {
		t.Fatalf("/devices implies floors %v, /floors lists %v", fromDevices, fromFloors)
	}
	for id, want := range fromDevices {
		got, ok := fromFloors[id]
		if !ok {
			t.Errorf("/devices puts devices on %q but /floors omits it", id)
			continue
		}
		if got != want {
			t.Errorf("%s device_count = %d, but /devices shows %d devices", id, got, want)
		}
	}
}

// An unknown floor is a 400 on /series, and /floors must never advertise one —
// asserted by driving a value /floors deliberately omits.
func TestIntegration_FloorsNeverAdvertisesARejectedValue(t *testing.T) {
	base, token, _ := floorsIntegrationServer(t)

	var body floorsBody
	get(t, base, token, "/floors", http.StatusOK, &body)

	listed := map[string]bool{}
	for _, f := range body.Floors {
		listed[f.ID] = true
	}

	// floor9 has a floorplan record but no climate device; floor4 holds only a
	// plug. Both are rejected by floors=, so both must be absent from /floors.
	for _, id := range []string{"floor9", "floor4"} {
		if listed[id] {
			t.Errorf("/floors advertises %q", id)
		}
		var errBody map[string]any
		get(t, base, token, "/series?window=today&interval=1h&floors="+id,
			http.StatusBadRequest, &errBody)
	}
}

// A floor the floorplan has no record for is still listed and still chartable,
// with its unknowns explicit — the case that would be silently dropped if the
// endpoint were built from the floorplan namespace instead of from the devices.
func TestIntegration_FloorWithNoRecordIsStillUsable(t *testing.T) {
	base, token, _ := floorsIntegrationServer(t)

	var body floorsBody
	get(t, base, token, "/floors", http.StatusOK, &body)

	var found bool
	for _, f := range body.Floors {
		if f.ID != "floor3" {
			continue
		}
		found = true
		if f.Name != "" || f.Order != nil {
			t.Errorf("floor3 = %q/%v, want explicit unknowns", f.Name, f.Order)
		}
	}
	if !found {
		t.Fatal("floor3 has a climate device and must be listed despite having no record")
	}

	var resp struct {
		Series []struct {
			Key string `json:"key"`
		} `json:"series"`
	}
	get(t, base, token, "/series?window=today&interval=1h&floors=floor3", http.StatusOK, &resp)
	if len(resp.Series) == 0 {
		t.Error("floor3 is listed but charts nothing")
	}
}

// With no floorplan namespace configured at all — the default for an instance
// that never sets one — /floors still works and still agrees with the filter.
// Floor labels are presentation detail and must never gate the climate API.
func TestIntegration_NoFloorplanConfiguredStillServesFloors(t *testing.T) {
	s, q := dataSetup(t)
	s.Config = fakeConfig{devices: catalogFloorDevices()}
	s.FloorRecords = nil // no floorplan_namespace set

	priv := genTestKey(t)
	const kid = "nofloorplan"
	id := fakeJWKSServer(t, &priv.PublicKey, kid)
	s.IdentityURL = id.URL
	q.QueryFunc = func(string) ([]influx.Row, error) {
		return bucketRows(t, s, "sensor_e", "today", "1h", 20), nil
	}

	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	token := serviceToken(t, priv, kid, id.URL, time.Now().Add(time.Hour))

	var body floorsBody
	get(t, srv.URL, token, "/floors", http.StatusOK, &body)

	var ids []string
	for _, f := range body.Floors {
		ids = append(ids, f.ID)
		if f.Name != "" || f.Order != nil {
			t.Errorf("%s carries %q/%v with no floorplan configured", f.ID, f.Name, f.Order)
		}
	}
	sort.Strings(ids)
	if strings.Join(ids, ",") != "floor1,floor2,floor3" {
		t.Errorf("floors = %v, want the climate floors listed regardless", ids)
	}
}

// The spec is how consumers find the endpoint, so the served document must carry
// the path and a schema whose unknowable fields are nullable.
func TestIntegration_SpecDocumentsFloorsEndpoint(t *testing.T) {
	base, _, _ := floorsIntegrationServer(t)

	var spec struct {
		Paths map[string]struct {
			Get struct {
				OperationID string `json:"operationId"`
			} `json:"get"`
		} `json:"paths"`
		Components struct {
			Schemas map[string]struct {
				Properties map[string]struct {
					Type     string `json:"type"`
					Nullable bool   `json:"nullable"`
				} `json:"properties"`
				Required []string `json:"required"`
			} `json:"schemas"`
		} `json:"components"`
	}
	get(t, base, "", "/openapi.json", http.StatusOK, &spec)

	p, ok := spec.Paths["/floors"]
	if !ok {
		t.Fatal("spec documents no /floors path")
	}
	if p.Get.OperationID != "getFloors" {
		t.Errorf("operationId = %q, want getFloors", p.Get.OperationID)
	}

	entry, ok := spec.Components.Schemas["FloorCatalogEntry"]
	if !ok {
		t.Fatal("spec defines no FloorCatalogEntry schema")
	}
	// order and elevation are unknowable, so they must be documented nullable —
	// a consumer generating types from this spec would otherwise get a non-null
	// int and fail to parse a real response.
	for _, field := range []string{"order", "elevation"} {
		prop, ok := entry.Properties[field]
		if !ok {
			t.Errorf("FloorCatalogEntry does not document %q", field)
			continue
		}
		if !prop.Nullable {
			t.Errorf("%q must be documented nullable: it is null whenever the floorplan "+
				"declares none", field)
		}
	}
	// All five are always present, so all five are required.
	for _, field := range []string{"id", "name", "order", "elevation", "device_count"} {
		var found bool
		for _, r := range entry.Required {
			if r == field {
				found = true
			}
		}
		if !found {
			t.Errorf("%q is always present in the response, so it must be required: %v",
				field, entry.Required)
		}
	}
}
