package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sweeney/greenhouse/internal/climate"
	"github.com/sweeney/greenhouse/internal/influx"
)

// This is the end-to-end test for the floor feature: a real net/http server, the
// real router, the real CORS and auth middleware, a real signed service token over
// the wire, and a fake Influx underneath. Everything above the Influx client is the
// production path.
//
// It exists because the floor is derived in TWO places that must agree — the `floor`
// the catalog publishes and the `floors=` value the series endpoint accepts. A unit
// test can pin each half; only driving the real API can show that a consumer can
// take a floor out of /devices and hand it straight back to /series.

// integrationServer boots the full handler stack behind a real listener, with auth
// enforced against a fake JWKS issuer. It returns the base URL, a service token
// good for that issuer, and the fake querier so tests can inspect the flux actually
// issued to Influx.
func integrationServer(t *testing.T) (base, token string, q *influx.FakeQuerier) {
	t.Helper()

	s, q := dataSetup(t)
	s.Config = fakeConfig{devices: floorDevices()}
	s.PublicURL = "https://greenhouse.swee.net"

	// Real JWKS verification: the token is signed here and fetched by the verifier.
	priv := genTestKey(t)
	const kid = "integrationkey"
	id := fakeJWKSServer(t, &priv.PublicKey, kid)
	s.IdentityURL = id.URL

	// Rows for every fixture device, so a series that IS charted has real values
	// and one that is filtered out is visibly absent rather than merely empty.
	q.QueryFunc = func(string) ([]influx.Row, error) {
		var rows []influx.Row
		for _, dev := range []string{
			"climate_kitchen", "climate_hall", "climate_drawingroom",
			"firealarm_utility", "climate_basement",
		} {
			rows = append(rows, bucketRows(t, s, dev, "today", "1h", 20)...)
		}
		return rows, nil
	}

	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)

	return srv.URL, serviceToken(t, priv, kid, id.URL, time.Now().Add(time.Hour)), q
}

// get performs a real HTTP GET with the bearer token and decodes the JSON body.
func get(t *testing.T, base, token, path string, want int, into any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != want {
		t.Fatalf("GET %s: want %d, got %d: %s", path, want, resp.StatusCode, body)
	}
	if into != nil {
		if err := json.Unmarshal(body, into); err != nil {
			t.Fatalf("GET %s: decode %s: %v", path, body, err)
		}
	}
}

// TestIntegration_FloorDiscoveryToChart walks the journey a real consumer makes:
// discover the devices, read the floors off the catalog, then chart a floor.
func TestIntegration_FloorDiscoveryToChart(t *testing.T) {
	base, token, q := integrationServer(t)

	// A token is genuinely required — the catalog is not public.
	get(t, base, "", "/devices", http.StatusUnauthorized, nil)

	var catalog struct {
		Devices []struct {
			ID    string `json:"id"`
			Room  string `json:"room"`
			Floor string `json:"floor"`
			Class string `json:"class"`
		} `json:"devices"`
	}
	get(t, base, token, "/devices", http.StatusOK, &catalog)

	// The catalog agrees with the floorplan: floor is the room id's leading
	// segment, and empty exactly when there is no room id to read it from.
	devicesByFloor := map[string][]string{}
	for _, d := range catalog.Devices {
		want, _, ok := strings.Cut(d.Room, ".")
		if !ok {
			want = ""
		}
		if d.Floor != want {
			t.Errorf("%s: floor = %q, want %q (from room %q)", d.ID, d.Floor, want, d.Room)
		}
		devicesByFloor[d.Floor] = append(devicesByFloor[d.Floor], d.ID)
	}
	if len(devicesByFloor[""]) != 1 {
		t.Errorf("want exactly the one unmigrated device to have no floor, got %v", devicesByFloor[""])
	}

	// Every floor the catalog publishes is chartable, and charts exactly the
	// devices the catalog put on it. This is the contract between the two
	// endpoints — a consumer never has to parse a room id itself.
	for floor, want := range devicesByFloor {
		if floor == "" {
			continue
		}
		t.Run("floor="+floor, func(t *testing.T) {
			q.Queries = nil

			var resp struct {
				GroupBy string `json:"group_by"`
				Series  []struct {
					Key    string    `json:"key"`
					Room   string    `json:"room"`
					Values []float64 `json:"values"`
				} `json:"series"`
			}
			get(t, base, token, "/series?window=today&interval=1h&floors="+floor, http.StatusOK, &resp)

			got := make([]string, 0, len(resp.Series))
			for _, ser := range resp.Series {
				got = append(got, ser.Key)
				if !strings.HasPrefix(ser.Room, floor+".") {
					t.Errorf("series %s has room %q, which is not on floor %q", ser.Key, ser.Room, floor)
				}
				if len(ser.Values) == 0 {
					t.Errorf("series %s carries no buckets", ser.Key)
				}
			}
			sort.Strings(got)
			sort.Strings(want)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("floors=%s charted %v, want the catalog's %v", floor, got, want)
			}

			// The filter reached Influx, not just the response shaping: the flux
			// device set names exactly this floor's devices. Filtering after the
			// query would look identical in the JSON and cost every other device.
			if len(q.Queries) != 1 {
				t.Fatalf("want exactly 1 flux query, got %d", len(q.Queries))
			}
			for _, d := range catalog.Devices {
				named := strings.Contains(q.Queries[0], `"`+d.ID+`"`)
				onFloor := d.Floor == floor
				if named != onFloor {
					t.Errorf("flux names %s = %v, want %v (floor %q)", d.ID, named, onFloor, floor)
				}
			}
		})
	}
}

// A floor nobody lives on is a client error over real HTTP too, with a JSON error
// body rather than net/http's plain-text default.
func TestIntegration_UnknownFloorIsAJSON400(t *testing.T) {
	base, token, _ := integrationServer(t)

	var body map[string]any
	get(t, base, token, "/series?window=today&interval=1h&floors=penthouse", http.StatusBadRequest, &body)

	msg, _ := body["error"].(string)
	if !strings.Contains(msg, "penthouse") || !strings.Contains(msg, "floors") {
		t.Errorf("error body should name the parameter and value, got %v", body)
	}
}

// The published spec describes what the server actually serves: floors= is a
// documented /series parameter, and floor is a required catalog field. The spec is
// how consumers find the feature, so drift here is a real defect.
func TestIntegration_SpecDocumentsFloors(t *testing.T) {
	base, _, _ := integrationServer(t)

	var spec struct {
		Paths map[string]struct {
			Get struct {
				Parameters []struct {
					Ref string `json:"$ref"`
				} `json:"parameters"`
			} `json:"get"`
		} `json:"paths"`
		Components struct {
			Parameters map[string]struct {
				Name string `json:"name"`
				In   string `json:"in"`
			} `json:"parameters"`
			Schemas map[string]struct {
				Properties map[string]any `json:"properties"`
				Required   []string       `json:"required"`
			} `json:"schemas"`
		} `json:"components"`
	}
	// The spec is public: no token.
	get(t, base, "", "/openapi.json", http.StatusOK, &spec)

	// Parameters are $refs, so resolve each one into components.parameters: a
	// dangling ref documents nothing, and asserting on the ref string alone would
	// pass against a component that does not exist.
	var found bool
	for _, p := range spec.Paths["/series"].Get.Parameters {
		name, ok := strings.CutPrefix(p.Ref, "#/components/parameters/")
		if !ok {
			continue
		}
		param, ok := spec.Components.Parameters[name]
		if !ok {
			t.Errorf("/series references %s, which is not defined", p.Ref)
			continue
		}
		if param.Name == "floors" && param.In == "query" {
			found = true
		}
	}
	if !found {
		t.Error("/series does not document a 'floors' query parameter")
	}

	entry := spec.Components.Schemas["DeviceCatalogEntry"]
	if _, ok := entry.Properties["floor"]; !ok {
		t.Error("DeviceCatalogEntry does not document 'floor'")
	}
	if !slicesContains(entry.Required, "floor") {
		t.Errorf("'floor' is always present in the response, so it must be required: %v", entry.Required)
	}
}

func slicesContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// The field registry is untouched by the floor work — a cheap guard that the new
// filter did not leak into an unrelated endpoint's contract.
func TestIntegration_FieldsUnaffected(t *testing.T) {
	base, token, _ := integrationServer(t)

	var resp struct {
		Fields []struct {
			Name string `json:"name"`
		} `json:"fields"`
	}
	get(t, base, token, "/fields", http.StatusOK, &resp)
	if len(resp.Fields) != len(climate.FieldNames()) {
		t.Errorf("fields = %d, want %d", len(resp.Fields), len(climate.FieldNames()))
	}
}
