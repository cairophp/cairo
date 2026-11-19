package sapi

import (
	"time"

	"github.com/cairophp/cairo/internal/wire"
)

// App::heavyRoutes(): the slow log already measures, for each request, the
// longest stretch of PHP its thread ran without coming back to Go
// (reqTiming.longest). Every other request on that thread waited for it, so
// past the threshold Go names the route, at most once a minute per route.

var heavy repeatedLog // the same once-a-minute limit as repeated queries

// logTiming logs what the request's timing shows once it has ended.
func (e *Engine) logTiming(ev wire.Event, t *reqTiming, end string) {
	e.logSlow(ev, t, end)
	e.logHeavy(ev, t)
}

func (e *Engine) logHeavy(ev wire.Event, t *reqTiming) {
	run := time.Duration(t.longest.Load())
	if e.heavyAfter == 0 || run < e.heavyAfter {
		return
	}
	var route string
	if n, ok := ev.(routeNamer); ok {
		route = n.RouteName()
	}
	if heavy.due(route, time.Now()) {
		e.log.Warn("heavy route: move its CPU work to offload()", "route", route, "longest_run_ms", run.Milliseconds())
	}
}
