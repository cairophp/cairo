package sapi

import (
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/cairophp/cairo/internal/core"
	"github.com/cairophp/cairo/internal/spec"
)

// fakeEngine is what startTiming needs of an engine: a declaration, and
// somewhere to keep the probe it asks for.
type fakeEngine struct {
	spec     spec.Spec
	newProbe func() core.Probe
}

func (e *fakeEngine) Spec() *spec.Spec                   { return &e.spec }
func (e *fakeEngine) ProbeRequests(fn func() core.Probe) { e.newProbe = fn }

// The engine is asked for a probe only when one of the two reports is on.
func TestStartTiming(t *testing.T) {
	for name, tc := range map[string]struct {
		section     string // what server.php declared; "" for nothing
		ok, probed  bool
		slow, heavy time.Duration
	}{
		"nothing declared": {"", true, false, 0, 0},
		"both off":         {`{"slow_requests":0,"heavy_routes":0}`, true, false, 0, 0},
		"slow requests":    {`{"slow_requests":0.5,"heavy_routes":0}`, true, true, 500 * time.Millisecond, 0},
		"heavy routes":     {`{"slow_requests":0,"heavy_routes":0.05}`, true, true, 0, 50 * time.Millisecond},
		"both":             {`{"slow_requests":1,"heavy_routes":0.05}`, true, true, time.Second, 50 * time.Millisecond},
		"negative":         {`{"slow_requests":-1,"heavy_routes":0.05}`, false, false, 0, 0},
		"not settings":     {`[]`, false, false, 0, 0},
	} {
		t.Run(name, func(t *testing.T) {
			e := &fakeEngine{}
			if tc.section != "" {
				e.spec.Extensions = map[string]json.RawMessage{"timing": json.RawMessage(tc.section)}
			}
			if err := startTiming(e, slog.Default()); (err == nil) != tc.ok {
				t.Fatalf("startTiming: %v, want ok %v", err, tc.ok)
			}
			if (e.newProbe != nil) != tc.probed {
				t.Fatalf("requests probed: %v, want %v", e.newProbe != nil, tc.probed)
			}
			if !tc.probed {
				return
			}
			r := e.newProbe().(*reqTiming)
			if r.timing.slowAfter != tc.slow || r.timing.heavyAfter != tc.heavy {
				t.Errorf("thresholds %s and %s, want %s and %s", r.timing.slowAfter, r.timing.heavyAfter, tc.slow, tc.heavy)
			}
			if r.start.IsZero() || e.newProbe() == core.Probe(r) {
				t.Error("each request needs its own probe, started when it is made")
			}
		})
	}
}
