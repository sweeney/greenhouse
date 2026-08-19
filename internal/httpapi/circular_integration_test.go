package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sweeney/greenhouse/internal/influx"
)

// End-to-end cover for the circular-field rule: a real listener, the real
// router, real CORS and auth middleware, a signed ES256 service token, and a
// fake Influx underneath. Everything above the Influx client is production code.
//
// It exists because the rule spans three layers that must agree — the field
// registry says which fields are angular, the handler decides whether a request
// can be answered, and the assembly step decides what a bucket contains. A unit
// test pins each; only the whole stack shows a consumer the same answer twice.

// windIntegrationServer boots the full handler stack behind a real listener with
// auth enforced, using the two-sensors-in-one-room fixture.
func windIntegrationServer(t *testing.T) (base, token string, q *influx.FakeQuerier) {
	t.Helper()

	s, q := dataSetup(t)
	s.Config = fakeConfig{devices: windDevices()}
	s.PublicURL = "https://greenhouse.swee.net"

	priv := genTestKey(t)
	const kid = "circularkey"
	id := fakeJWKSServer(t, &priv.PublicKey, kid)
	s.IdentityURL = id.URL

	// probe_a reads 350°, probe_b reads 10° — both essentially due North, and the
	// arithmetic mean of them is 180°, due South. That number is what must never
	// reach a consumer.
	q.QueryFunc = func(string) ([]influx.Row, error) {
		rows := bucketRows(t, s, "probe_a", "today", "1h", 350)
		rows = append(rows, bucketRows(t, s, "probe_b", "today", "1h", 10)...)
		rows = append(rows, bucketRows(t, s, "outdoor_station", "today", "1h", 180)...)
		return rows, nil
	}

	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)

	return srv.URL, serviceToken(t, priv, kid, id.URL, time.Now().Add(time.Hour)), q
}

// The journey a consumer makes: chart a bearing per device (fine), then try to
// group it by room (refused, with a reason), and confirm the refused number
// never appears anywhere in the response.
func TestIntegration_CircularBearingIsNeverAveraged(t *testing.T) {
	base, token, _ := windIntegrationServer(t)

	// A token is genuinely required for the data routes.
	get(t, base, "", "/series?window=today&interval=1h&field=wind_dir_deg",
		http.StatusUnauthorized, nil)

	// Per device: every bearing is charted, and each is the sensor's own reading.
	var perDevice struct {
		Series []struct {
			Key    string     `json:"key"`
			Values []*float64 `json:"values"`
			Min    *float64   `json:"min"`
			Max    *float64   `json:"max"`
			Mean   *float64   `json:"mean"`
		} `json:"series"`
	}
	get(t, base, token,
		"/series?window=today&interval=1h&field=wind_dir_deg&group_by=device",
		http.StatusOK, &perDevice)

	want := map[string]float64{"probe_a": 350, "probe_b": 10, "outdoor_station": 180}
	if len(perDevice.Series) != len(want) {
		t.Fatalf("got %d series, want %d", len(perDevice.Series), len(want))
	}
	for _, ser := range perDevice.Series {
		w, ok := want[ser.Key]
		if !ok {
			t.Fatalf("unexpected series %s", ser.Key)
		}
		if len(ser.Values) == 0 || ser.Values[0] == nil || *ser.Values[0] != w {
			t.Errorf("%s: values[0] = %v, want the sensor's own bearing %v",
				ser.Key, ser.Values[0], w)
		}
		// Linear summaries of a bearing are meaningless, so they are null.
		if ser.Min != nil || ser.Max != nil || ser.Mean != nil {
			t.Errorf("%s: summary stats must be null for a bearing, got %v/%v/%v",
				ser.Key, ser.Min, ser.Max, ser.Mean)
		}
	}

	// Grouped by room, probe_a and probe_b would meet. That is refused, and the
	// error says which field, which room, and what to do instead.
	var errBody map[string]any
	get(t, base, token,
		"/series?window=today&interval=1h&field=wind_dir_deg&group_by=room",
		http.StatusBadRequest, &errBody)

	msg, _ := errBody["error"].(string)
	for _, want := range []string{"wind_dir_deg", "floor1.room-a", "group_by=device"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should mention %q, got %q", want, msg)
		}
	}
	// The whole point: the average of 350 and 10 must not be anywhere in the reply.
	if strings.Contains(msg, "180") {
		t.Errorf("the refused average leaked into the error body: %q", msg)
	}
}

// Narrowing to one sensor per room makes the same grouping answerable, over the
// real stack. The way out the error advertises has to actually exist.
func TestIntegration_CircularGroupingWorksOnceNarrowed(t *testing.T) {
	base, token, _ := windIntegrationServer(t)

	var resp struct {
		Series []struct {
			Key    string     `json:"key"`
			Values []*float64 `json:"values"`
		} `json:"series"`
	}
	get(t, base, token,
		"/series?window=today&interval=1h&field=wind_dir_deg&group_by=room&devices=probe_a,outdoor_station",
		http.StatusOK, &resp)

	if len(resp.Series) != 2 {
		t.Fatalf("want one series per room, got %d", len(resp.Series))
	}
	for _, ser := range resp.Series {
		if len(ser.Values) == 0 || ser.Values[0] == nil {
			t.Fatalf("%s carries no reading", ser.Key)
		}
		if *ser.Values[0] == 180 && ser.Key == "floor1.room-a" {
			t.Errorf("%s = 180: that is the forbidden average, not a real reading", ser.Key)
		}
	}
}

// A temperature across the very same two co-located sensors is a legitimate
// mean, and still is. The fix must be scoped to the angular field, not to
// grouping in general.
func TestIntegration_LinearFieldStillGroups(t *testing.T) {
	base, token, _ := windIntegrationServer(t)

	var resp struct {
		Series []struct {
			Key    string     `json:"key"`
			Values []*float64 `json:"values"`
			Mean   *float64   `json:"mean"`
		} `json:"series"`
	}
	get(t, base, token,
		"/series?window=today&interval=1h&field=temperature_c&group_by=room",
		http.StatusOK, &resp)

	var found bool
	for _, ser := range resp.Series {
		if ser.Key != "floor1.room-a" {
			continue
		}
		found = true
		// The fake returns the same values whatever the field, so the room's two
		// members still mean to 180 — proving the mean itself is untouched and it
		// is only the circular flag that suppresses it.
		if ser.Values[0] == nil || *ser.Values[0] != 180 {
			t.Errorf("want the mean of 350 and 10 for a linear field, got %v", ser.Values[0])
		}
		if ser.Mean == nil {
			t.Error("a linear field keeps its summary stats")
		}
	}
	if !found {
		t.Error("no series for the shared room")
	}
}

// The spec is how consumers learn the rule, so it must say that a circular field
// cannot be grouped across members rather than leaving them to discover the 400.
func TestIntegration_SpecDocumentsTheCircularGroupingRule(t *testing.T) {
	base, _, _ := windIntegrationServer(t)

	var spec struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]struct {
					Description string `json:"description"`
				} `json:"properties"`
			} `json:"schemas"`
			Parameters map[string]struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"parameters"`
		} `json:"components"`
	}
	get(t, base, "", "/openapi.json", http.StatusOK, &spec)

	gb, ok := spec.Components.Parameters["GroupBy"]
	if !ok {
		t.Fatal("spec defines no GroupBy parameter")
	}
	if !strings.Contains(gb.Description, "circular") {
		t.Errorf("GroupBy should document the circular-field restriction, got %q",
			gb.Description)
	}
}
