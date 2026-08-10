package httpapi

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/sweeney/greenhouse/internal/influx"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite the golden alias snapshots")

// These snapshots pinned the deprecated `location` spelling until it was removed in
// step 11 of the floorplan migration. The alias cases were deleted deliberately, and
// their golden files with them; what remains pins the room responses.
func TestGoldenDeprecatedAliasResponses(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"group-by-room", "/series?window=today&interval=1h&group_by=room"},
		{"rooms-filter", "/series?window=today&interval=1h&rooms=basement.hallway"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, q := dataSetup(t)
			s.Config = fakeConfig{devices: roomDevices()}
			q.QueryFunc = func(string) ([]influx.Row, error) {
				return bucketRows(t, s, "climate_basement", "today", "1h", 20), nil
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
				t.Errorf("%s drifted from its golden snapshot.\nIf this is a deliberate "+
					"alias removal, regenerate with -update-golden.\n got: %s\nwant: %s",
					tc.path, formatted, want)
			}
		})
	}
}
