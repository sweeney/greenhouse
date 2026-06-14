package climate

import "testing"

func TestFieldFor(t *testing.T) {
	cases := []struct {
		name      string
		unit      string
		defaultFn string
	}{
		{"temperature_c", "°C", "mean"},
		{"humidity_pct", "%", "mean"},
		{"pressure_hpa", "hPa", "mean"},
		{"wind_speed_ms", "m/s", "mean"},
		// wind_dir_deg is circular: arithmetic mean is wrong, so it defaults to last.
		{"wind_dir_deg", "°", "last"},
		{"rainfall_mm", "mm", "mean"},
		{"illuminance_lux", "lux", "mean"},
		{"uv_index", "index", "mean"},
	}
	for _, c := range cases {
		f, ok := FieldFor(c.name)
		if !ok {
			t.Errorf("FieldFor(%q) not found", c.name)
			continue
		}
		if f.Unit != c.unit {
			t.Errorf("FieldFor(%q).Unit = %q, want %q", c.name, f.Unit, c.unit)
		}
		if f.DefaultFn != c.defaultFn {
			t.Errorf("FieldFor(%q).DefaultFn = %q, want %q", c.name, f.DefaultFn, c.defaultFn)
		}
	}
}

// TestWindDirIsCircular pins the fix for wind_dir_deg: it is an angular 0–360°
// quantity, so arithmetic mean/min/max are mathematically wrong (mean(350,10)
// = 180 = South, when the true average is ~0 = North). The field is marked
// circular, defaults to last (a single instantaneous bearing is always valid),
// and the linear aggregations are rejected via ValidFnForField.
func TestWindDirIsCircular(t *testing.T) {
	f, ok := FieldFor("wind_dir_deg")
	if !ok {
		t.Fatal("wind_dir_deg should be a known field")
	}
	if !f.Circular {
		t.Error("wind_dir_deg must be marked circular")
	}
	if f.DefaultFn != "last" {
		t.Errorf("wind_dir_deg DefaultFn = %q, want last", f.DefaultFn)
	}
	for _, fn := range []string{"mean", "min", "max"} {
		if ValidFnForField(f, fn) {
			t.Errorf("ValidFnForField(wind_dir_deg, %q) = true, want false (circular)", fn)
		}
	}
	if !ValidFnForField(f, "last") {
		t.Error("last must be valid for wind_dir_deg")
	}
	if ValidFnForField(f, "sum") {
		t.Error("sum must never be valid (non-additive), circular or not")
	}
}

// TestValidFnForField_NonCircular confirms ordinary gauges keep the full
// mean/min/max/last set and still reject sum.
func TestValidFnForField_NonCircular(t *testing.T) {
	temp, _ := FieldFor("temperature_c")
	if temp.Circular {
		t.Fatal("temperature_c must not be circular")
	}
	for _, fn := range []string{"mean", "min", "max", "last"} {
		if !ValidFnForField(temp, fn) {
			t.Errorf("temperature_c should allow %q", fn)
		}
	}
	if ValidFnForField(temp, "sum") {
		t.Error("sum stays forbidden for temperature_c")
	}
}

func TestFieldFor_Unknown(t *testing.T) {
	if _, ok := FieldFor("co2_ppm"); ok {
		t.Error("FieldFor(co2_ppm) should be unknown")
	}
	if _, ok := FieldFor(""); ok {
		t.Error("FieldFor(empty) should be unknown")
	}
}

func TestValidFn(t *testing.T) {
	for _, fn := range []string{"mean", "min", "max", "last"} {
		if !ValidFn(fn) {
			t.Errorf("ValidFn(%q) = false, want true", fn)
		}
	}
	for _, fn := range []string{"sum", "median", "count", ""} {
		if ValidFn(fn) {
			t.Errorf("ValidFn(%q) = true, want false (sum is non-additive-forbidden)", fn)
		}
	}
}

func TestAllowedFnsExcludesSum(t *testing.T) {
	for _, fn := range AllowedFns() {
		if fn == "sum" {
			t.Fatal("sum must not be an allowed climate fn (non-additive)")
		}
	}
}

func TestFieldsRegistryCount(t *testing.T) {
	if got := len(Fields()); got != 8 {
		t.Errorf("Fields() len = %d, want 8", got)
	}
	if got := len(FieldNames()); got != 8 {
		t.Errorf("FieldNames() len = %d, want 8", got)
	}
	// Sorted.
	names := FieldNames()
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Errorf("FieldNames not sorted: %v", names)
			break
		}
	}
}

func TestDefaultFieldAndFn(t *testing.T) {
	if DefaultField != "temperature_c" {
		t.Errorf("DefaultField = %q, want temperature_c", DefaultField)
	}
	if DefaultFn != "mean" {
		t.Errorf("DefaultFn = %q, want mean", DefaultFn)
	}
}
