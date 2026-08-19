package httpapi

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sweeney/greenhouse/internal/influx"
)

// End-to-end cover for floor grouping and the caller-chosen member combine: a
// real listener, the real router, real CORS and auth middleware, a signed ES256
// service token, and a fake Influx underneath.
//
// It exists because the feature's value is a JOURNEY across endpoints — read the
// floors off /devices, chart each as a band from /series — and because the two
// aggregation axes must stay on their own sides of the Influx boundary. A unit
// test can pin each half; only the real stack shows they agree.

func groupIntegrationServer(t *testing.T) (base, token string, q *influx.FakeQuerier) {
	t.Helper()

	s, q := dataSetup(t)
	s.Config = fakeConfig{devices: groupDevices()}
	s.PublicURL = "https://greenhouse.swee.net"

	priv := genTestKey(t)
	const kid = "groupfnkey"
	id := fakeJWKSServer(t, &priv.PublicKey, kid)
	s.IdentityURL = id.URL

	values := map[string]float64{
		"sensor_e": 10, "sensor_f": 20, "sensor_g": 30, "sensor_h": 5, "sensor_x": 99,
	}
	q.QueryFunc = func(string) ([]influx.Row, error) {
		var rows []influx.Row
		for id, v := range values {
			rows = append(rows, bucketRows(t, s, id, "today", "1h", v)...)
		}
		return rows, nil
	}

	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)

	return srv.URL, serviceToken(t, priv, kid, id.URL, time.Now().Add(time.Hour)), q
}

type bandSeries struct {
	Key    string     `json:"key"`
	Room   string     `json:"room"`
	Values []*float64 `json:"values"`
}

type bandResponse struct {
	GroupBy string       `json:"group_by"`
	GroupFn string       `json:"group_fn"`
	Fn      string       `json:"fn"`
	Series  []bandSeries `json:"series"`
}

func (b bandResponse) first(key string) (float64, bool) {
	for _, s := range b.Series {
		if s.Key == key && len(s.Values) > 0 && s.Values[0] != nil {
			return *s.Values[0], true
		}
	}
	return 0, false
}

// The journey the feature exists for: discover the floors, then render one as a
// min–max band with the mean through it. A floor spanning a warm room and a cold
// one shows up as the band's WIDTH — the information a single mean destroys.
func TestIntegration_FloorRendersAsABand(t *testing.T) {
	base, token, _ := groupIntegrationServer(t)

	// A token is genuinely required.
	get(t, base, "", "/series?window=today&interval=1h&group_by=floor",
		http.StatusUnauthorized, nil)

	// Read the floors off the catalog, exactly as a consumer would.
	var catalog struct {
		Devices []struct {
			ID    string `json:"id"`
			Floor string `json:"floor"`
		} `json:"devices"`
	}
	get(t, base, token, "/devices", http.StatusOK, &catalog)

	floors := map[string]bool{}
	for _, d := range catalog.Devices {
		if d.Floor != "" {
			floors[d.Floor] = true
		}
	}
	if !floors["floor1"] {
		t.Fatalf("catalog published no floor1: %v", catalog.Devices)
	}

	// Chart floor1 three ways and assemble the band.
	band := map[string]float64{}
	for _, fn := range []string{"min", "mean", "max"} {
		var resp bandResponse
		get(t, base, token,
			"/series?window=today&interval=1h&group_by=floor&group_fn="+fn,
			http.StatusOK, &resp)

		if resp.GroupBy != "floor" {
			t.Errorf("group_by echoed as %q, want floor", resp.GroupBy)
		}
		if resp.GroupFn != fn {
			t.Errorf("group_fn echoed as %q, want %q", resp.GroupFn, fn)
		}
		v, ok := resp.first("floor1")
		if !ok {
			t.Fatalf("group_fn=%s: no floor1 series", fn)
		}
		band[fn] = v
	}

	if band["min"] != 10 || band["mean"] != 20 || band["max"] != 30 {
		t.Errorf("band = min %v / mean %v / max %v, want 10/20/30",
			band["min"], band["mean"], band["max"])
	}
	if band["max"]-band["min"] <= 0 {
		t.Error("the band has no width, so it conveys nothing a mean would not")
	}
}

// Every floor the catalog publishes is chartable, and charts exactly the devices
// the catalog put on it — the contract between the two endpoints, now on the
// grouping axis rather than only the filter axis.
func TestIntegration_EveryPublishedFloorIsGroupable(t *testing.T) {
	base, token, q := groupIntegrationServer(t)

	var catalog struct {
		Devices []struct {
			ID    string `json:"id"`
			Floor string `json:"floor"`
		} `json:"devices"`
	}
	get(t, base, token, "/devices", http.StatusOK, &catalog)

	onFloor := map[string][]string{}
	for _, d := range catalog.Devices {
		if d.Floor != "" {
			onFloor[d.Floor] = append(onFloor[d.Floor], d.ID)
		}
	}

	for floor, want := range onFloor {
		t.Run(floor, func(t *testing.T) {
			q.Queries = nil
			var resp bandResponse
			get(t, base, token,
				"/series?window=today&interval=1h&group_by=floor&floors="+floor,
				http.StatusOK, &resp)

			if len(resp.Series) != 1 || resp.Series[0].Key != floor {
				t.Fatalf("want exactly the one floor series, got %v", resp.Series)
			}
			// The filter reached Influx: the flux names this floor's devices and no
			// others, so grouping is not paid for by querying the whole house.
			if len(q.Queries) != 1 {
				t.Fatalf("want 1 flux query, got %d", len(q.Queries))
			}
			for _, d := range catalog.Devices {
				named := strings.Contains(q.Queries[0], `"`+d.ID+`"`)
				expected := false
				for _, id := range want {
					if id == d.ID {
						expected = true
					}
				}
				if named != expected {
					t.Errorf("flux names %s = %v, want %v for floor %q", d.ID, named, expected, floor)
				}
			}
		})
	}
}

// The axes do not commute, and the API keeps them apart across the real stack:
// fn=max&group_fn=mean ("the mean of each member's peak") and
// fn=mean&group_fn=max ("the warmest member's bucket average") both answer, and
// only fn reaches Influx.
func TestIntegration_AxesAreIndependentAndOrdered(t *testing.T) {
	base, token, q := groupIntegrationServer(t)

	for _, tc := range []struct{ fn, groupFn string }{
		{"max", "mean"},
		{"mean", "max"},
	} {
		t.Run(tc.fn+"/"+tc.groupFn, func(t *testing.T) {
			q.Queries = nil
			var resp bandResponse
			get(t, base, token,
				"/series?window=today&interval=1h&group_by=floor&fn="+tc.fn+"&group_fn="+tc.groupFn,
				http.StatusOK, &resp)

			if resp.Fn != tc.fn || resp.GroupFn != tc.groupFn {
				t.Errorf("echoed %s/%s, want %s/%s", resp.Fn, resp.GroupFn, tc.fn, tc.groupFn)
			}
			if len(q.Queries) != 1 {
				t.Fatalf("want 1 flux query, got %d", len(q.Queries))
			}
			// fn is the Influx-side aggregate; group_fn is a Go-side step and must
			// not have rewritten the query.
			if !strings.Contains(q.Queries[0], tc.fn) {
				t.Errorf("flux should carry fn=%s, got %s", tc.fn, q.Queries[0])
			}
			if strings.Contains(q.Queries[0], "group_fn") {
				t.Errorf("group_fn leaked into the flux: %s", q.Queries[0])
			}
		})
	}
}

// Omitting group_fn must leave a response byte-identical to what greenhouse
// served before the parameter existed, apart from the echoed default.
func TestIntegration_OmittingGroupFnIsTheOldBehaviour(t *testing.T) {
	base, token, _ := groupIntegrationServer(t)

	var bare, explicit bandResponse
	get(t, base, token, "/series?window=today&interval=1h&group_by=room",
		http.StatusOK, &bare)
	get(t, base, token, "/series?window=today&interval=1h&group_by=room&group_fn=mean",
		http.StatusOK, &explicit)

	if len(bare.Series) != len(explicit.Series) {
		t.Fatalf("series counts differ: %d vs %d", len(bare.Series), len(explicit.Series))
	}
	for i := range bare.Series {
		b, e := bare.Series[i], explicit.Series[i]
		if b.Key != e.Key || len(b.Values) != len(e.Values) {
			t.Fatalf("series %d differs: %v vs %v", i, b, e)
		}
		for j := range b.Values {
			if (b.Values[j] == nil) != (e.Values[j] == nil) {
				t.Errorf("%s bucket %d nullity differs", b.Key, j)
				continue
			}
			if b.Values[j] != nil && *b.Values[j] != *e.Values[j] {
				t.Errorf("%s bucket %d = %v vs %v", b.Key, j, *b.Values[j], *e.Values[j])
			}
		}
	}
	if bare.GroupFn != "mean" {
		t.Errorf("bare group_fn echoed as %q, want the default mean", bare.GroupFn)
	}
}

// A device declaring no floor is UNKNOWN and is absent from the floor view over
// the real stack — never keyed on "", never folded into someone else's storey.
func TestIntegration_UndeclaredFloorIsAbsentNotEmptyKeyed(t *testing.T) {
	base, token, _ := groupIntegrationServer(t)

	var resp bandResponse
	get(t, base, token, "/series?window=today&interval=1h&group_by=floor",
		http.StatusOK, &resp)

	for _, s := range resp.Series {
		if s.Key == "" {
			t.Error(`a series was keyed on "", which is not a valid floor id`)
		}
		for _, v := range s.Values {
			if v != nil && *v == 99 {
				t.Errorf("floor %q absorbed sensor_x, which declares no floor", s.Key)
			}
		}
	}

	// It is still chartable per device, which is where the README points.
	var perDevice bandResponse
	get(t, base, token, "/series?window=today&interval=1h&group_by=device",
		http.StatusOK, &perDevice)
	if v, ok := perDevice.first("sensor_x"); !ok || v != 99 {
		t.Errorf("sensor_x per-device = %v (found=%v), want its own reading", v, ok)
	}
}

// The rejections hold over real HTTP with JSON error bodies, not net/http's
// plain-text default.
func TestIntegration_GroupFnRejectionsAreJSON400s(t *testing.T) {
	base, token, _ := groupIntegrationServer(t)

	for _, tc := range []struct {
		name, query, wantIn string
	}{
		{"last across members", "group_by=floor&group_fn=last", "last"},
		{"identity case", "group_by=device&group_fn=mean", "group_fn"},
		{"unknown combine", "group_by=floor&group_fn=sum", "group_fn"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			get(t, base, token, "/series?window=today&interval=1h&"+tc.query,
				http.StatusBadRequest, &body)
			msg, _ := body["error"].(string)
			if !strings.Contains(msg, tc.wantIn) {
				t.Errorf("error should mention %q, got %q", tc.wantIn, msg)
			}
		})
	}
}

// The spec is how consumers find the feature, so it must document floor as a
// group_by value and group_fn as a parameter — with its enum excluding last.
func TestIntegration_SpecDocumentsFloorGroupingAndGroupFn(t *testing.T) {
	base, _, _ := groupIntegrationServer(t)

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
				Name   string `json:"name"`
				In     string `json:"in"`
				Schema struct {
					Enum []string `json:"enum"`
				} `json:"schema"`
			} `json:"parameters"`
		} `json:"components"`
	}
	get(t, base, "", "/openapi.json", http.StatusOK, &spec)

	gb, ok := spec.Components.Parameters["GroupBy"]
	if !ok {
		t.Fatal("spec defines no GroupBy parameter")
	}
	if !slices.Contains(gb.Schema.Enum, "floor") {
		t.Errorf("group_by enum = %v, want floor among them", gb.Schema.Enum)
	}

	// group_fn must be referenced by /series, not merely defined.
	var referenced bool
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
		if param.Name == "group_fn" && param.In == "query" {
			referenced = true
			if slices.Contains(param.Schema.Enum, "last") {
				t.Errorf("group_fn enum offers last, which the API rejects: %v", param.Schema.Enum)
			}
			if !slices.Contains(param.Schema.Enum, "mean") {
				t.Errorf("group_fn enum = %v, want mean among them", param.Schema.Enum)
			}
		}
	}
	if !referenced {
		t.Error("/series does not document a 'group_fn' query parameter")
	}
}
