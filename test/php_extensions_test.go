package test

import (
	"encoding/json"
	"os"
	"testing"
)

// TestPHPBuildHasOpcacheAndExtensions checks the default build (scripts/build-php.sh):
// opcache runs under the cairo SAPI and the wider extension set is loaded.
func TestPHPBuildHasOpcacheAndExtensions(t *testing.T) {
	raw := get(t, "/php/extensions")
	var out struct {
		SAPI       string          `json:"sapi"`
		Pcntl      []string        `json:"pcntl"`
		Extensions map[string]bool `json:"extensions"`
		Opcache    struct {
			Loaded, Enabled, Cached bool
			Runtime                 map[string]bool
		} `json:"opcache"`
		Checks map[string]any `json:"checks"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	if !out.Extensions["mbstring"] {
		t.Skip("PHP was built before the wider extension set; run make php again")
	}
	if out.SAPI != "cairo" {
		t.Fatalf("PHP_SAPI = %q, want cairo", out.SAPI)
	}
	if len(out.Pcntl) != 0 {
		t.Errorf("a server has pcntl functions: %v", out.Pcntl)
	}
	for name, ok := range out.Extensions {
		if !ok {
			t.Errorf("extension %s is not loaded", name)
		}
	}
	// The static binary has no opcache before PHP 8.5 (scripts/build-static.sh).
	if os.Getenv("CAIRO_TEST_NO_OPCACHE") == "" && (!out.Opcache.Loaded || !out.Opcache.Enabled || !out.Opcache.Cached) {
		t.Errorf("opcache = %+v, want loaded, enabled and caching the route file", out.Opcache)
	}
	// Cairo writes its own PHP files when it starts. Opcache has to hold
	// them all the same, or every PHP thread compiles them for itself.
	for _, part := range []string{"core", "extension", "onUse"} {
		if os.Getenv("CAIRO_TEST_NO_OPCACHE") == "" && !out.Opcache.Runtime[part] {
			t.Errorf("opcache does not hold Cairo's own PHP file (%s): %+v", part, out.Opcache.Runtime)
		}
	}
	want := map[string]any{
		"mb_strtoupper": "ÜMLAUT", "bcadd": "0.3", "sodium": float64(32), "intl": "1,234,567.5", "sqlite": float64(42),
	}
	for k, v := range want {
		if out.Checks[k] != v {
			t.Errorf("%s = %#v, want %#v", k, out.Checks[k], v)
		}
	}
}
