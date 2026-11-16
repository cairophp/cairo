package test

import (
	"encoding/json"
	"testing"
)

// TestPHPBuildHasOpcacheAndExtensions checks the default build (scripts/build-php.sh):
// opcache runs under the embed SAPI and the wider extension set is loaded.
func TestPHPBuildHasOpcacheAndExtensions(t *testing.T) {
	raw := get(t, "/php/extensions")
	var out struct {
		SAPI       string          `json:"sapi"`
		Extensions map[string]bool `json:"extensions"`
		Opcache    struct {
			Loaded, Enabled, Cached bool
		} `json:"opcache"`
		Checks map[string]any `json:"checks"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	if !out.Extensions["mbstring"] {
		t.Skip("PHP was built before the wider extension set; run make php again")
	}
	if out.SAPI != "embed" {
		t.Fatalf("PHP_SAPI = %q, want embed", out.SAPI)
	}
	for name, ok := range out.Extensions {
		if !ok {
			t.Errorf("extension %s is not loaded", name)
		}
	}
	if !out.Opcache.Loaded || !out.Opcache.Enabled || !out.Opcache.Cached {
		t.Errorf("opcache = %+v, want loaded, enabled and caching the route file", out.Opcache)
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
