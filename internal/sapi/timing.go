package sapi

import (
	"errors"
	"log/slog"
	"time"

	"github.com/cairophp/cairo/internal/wire"
)

// The request timing behind App::slowRequests() and App::heavyRoutes(): one
// reqTiming per request, which the engine fills in as the request's probe
// and both reports read when the request is done. With neither report on,
// the engine is asked for no probe and no request is timed.

// timing is the two reports as server.php declared them (0: off).
type timing struct {
	slowAfter  time.Duration
	heavyAfter time.Duration
	log        *slog.Logger
}

func init() { registerStart(startTiming) }

func startTiming(e *Engine) error {
	var declared struct { // in seconds
		SlowRequests float64 `json:"slow_requests"`
		HeavyRoutes  float64 `json:"heavy_routes"`
	}
	if err := e.spec.Section("timing", &declared); err != nil {
		return err
	}
	if declared.SlowRequests < 0 || declared.HeavyRoutes < 0 {
		return errors.New("slowRequests and heavyRoutes must be 0 or more")
	}
	tm := &timing{
		slowAfter:  time.Duration(declared.SlowRequests * float64(time.Second)),
		heavyAfter: time.Duration(declared.HeavyRoutes * float64(time.Second)),
		log:        e.log,
	}
	if tm.slowAfter > 0 || tm.heavyAfter > 0 {
		e.probeRequests(func() probe { return &reqTiming{timing: tm, start: time.Now()} })
	}
	return nil
}

// done logs what the request's timing shows once it has ended.
func (t *reqTiming) done(ev wire.Event, end string) {
	t.timing.logSlow(ev, t, end)
	t.timing.logHeavy(ev, t)
}
