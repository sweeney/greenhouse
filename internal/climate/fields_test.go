package climate

import "testing"

func TestFieldFor(t *testing.T) {
	cases := []struct {
		name string
		unit string
	}{
		{"temperature_c", "°C"},
		{"humidity_pct", "%"},
		{"pressure_hpa", "hPa"},
		{"wind_speed_ms", "m/s"},
		{"wind_dir_deg", "°"},
		{"rainfall_mm", "mm"},
		{"illuminance_lux", "lux"},
		{"uv_index", "index"},
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
		if f.DefaultFn != "mean" {
			t.Errorf("FieldFor(%q).DefaultFn = %q, want mean", c.name, f.DefaultFn)
		}
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
