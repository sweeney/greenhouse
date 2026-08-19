package httpapi

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/sweeney/greenhouse/internal/influx"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite the golden series snapshots")

// These snapshots pin the room responses: series keys, envelope shape and computed
// values. They began as a tripwire for the deprecated `location` spelling, and those
// cases were deleted deliberately when it was removed rather than regenerated past.
//
// The failure message matters as much as the assertion. Telling the next engineer to
// reach for -update-golden is how a real regression gets blessed away, so it says what
// these actually guard instead.
func TestGoldenSeriesResponses(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"group-by-room", "/series?window=today&interval=1h&group_by=room"},
		{"rooms-filter", "/series?window=today&interval=1h&rooms=floor1.room-a"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, q := dataSetup(t)
			s.Config = fakeConfig{devices: roomDevices()}
			q.QueryFunc = func(string) ([]influx.Row, error) {
				return bucketRows(t, s, "sensor_a", "today", "1h", 20), nil
			}

			got := doGET(t, s, tc.path).Body.Bytes()

			var pretty any
			if err := json.Unmarshal(got, &pretty); err != nil {
				t.Fatalf("response is not JSON: %v", err)
			}
			formatted, err := json.MarshalIndent(pretty, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			formatted = append(formatted, '\n')

			path := filepath.Join("testdata", tc.name+".golden.json")
			if *updateGolden {
				if err := os.WriteFile(path, formatted, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}

			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v\nrun: go test ./internal/httpapi -update-golden", err)
			}
			if string(formatted) != string(want) {
				t.Errorf("%s drifted from its golden snapshot.\nThese pin computed values, "+
					"not just shape — regenerate only if the change is intended.\n got: %s\nwant: %s",
					tc.path, formatted, want)
			}
		})
	}
}
