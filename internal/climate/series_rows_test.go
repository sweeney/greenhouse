package climate

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func sampleResponse() SeriesResponse {
	base := time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC)
	buckets := []time.Time{base, base.Add(time.Hour), base.Add(2 * time.Hour)}
	return SeriesResponse{
		Window:   "today",
		From:     "2026-06-11T00:00:00Z",
		To:       "2026-06-11T03:00:00Z",
		Interval: "1h",
		GroupBy:  "device",
		Field:    "temperature_c",
		Unit:     "°C",
		Fn:       "mean",
		Shape:    ShapeColumns,
		Buckets:  buckets,
		Series: []Series{
			{Key: "climate_basement", Label: "Basement", Room: "basement",
				Values: []float64{18, math.NaN(), 20}, Min: 18, Max: 20, Mean: 19},
		},
	}
}

func TestRows_Reshape(t *testing.T) {
	resp := sampleResponse()
	rows := resp.Rows()

	if rows.Shape != ShapeRows {
		t.Errorf("shape = %q, want rows", rows.Shape)
	}
	// Metadata preserved.
	if rows.Field != "temperature_c" || rows.Unit != "°C" || rows.Fn != "mean" {
		t.Errorf("metadata lost: field=%q unit=%q fn=%q", rows.Field, rows.Unit, rows.Fn)
	}
	if len(rows.Series) != 1 {
		t.Fatalf("want 1 series meta, got %d", len(rows.Series))
	}
	// One row per (series, bucket): 1 * 3.
	if len(rows.Rows) != 3 {
		t.Fatalf("want 3 rows, got %d", len(rows.Rows))
	}
	if rows.Rows[0].Value != 18 || rows.Rows[0].Key != "climate_basement" {
		t.Errorf("row 0 = %+v", rows.Rows[0])
	}
	if !rows.Rows[0].Time.Equal(resp.Buckets[0]) {
		t.Errorf("row 0 time = %v, want %v", rows.Rows[0].Time, resp.Buckets[0])
	}
	if !math.IsNaN(rows.Rows[1].Value) {
		t.Errorf("row 1 should preserve NaN gap, got %v", rows.Rows[1].Value)
	}
}

func TestRows_JSONNullGap(t *testing.T) {
	resp := sampleResponse()
	b, err := json.Marshal(resp.Rows())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := string(b)
	if !strings.Contains(out, `"value":null`) {
		t.Errorf("gap row should marshal value null: %s", out)
	}
	if !strings.Contains(out, `"value":18`) {
		t.Errorf("real value missing: %s", out)
	}
}

func TestValidShape(t *testing.T) {
	for _, s := range []string{"", "columns", "rows"} {
		if !ValidShape(s) {
			t.Errorf("ValidShape(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"wide", "long", "json"} {
		if ValidShape(s) {
			t.Errorf("ValidShape(%q) = true, want false", s)
		}
	}
}
