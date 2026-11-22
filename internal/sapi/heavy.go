package sapi

import (
	"time"

	"github.com/cairophp/cairo/internal/wire"
)

// App::heavyRoutes(): the slow log already measures, for each request, the
// longest stretch of PHP its thread ran without coming back to Go
// (reqTiming.longest). Every other request on that thread waited for it, so
// past the threshold Go names the route, at most once a minute per route.

var heavy logOnce

func (tm *timing) logHeavy(ev wire.Event, t *reqTiming) {
	run := time.Duration(t.longest.Load())
	if tm.heavyAfter == 0 || run < tm.heavyAfter {
		return
	}
	var route string
	if n, ok := ev.(routeNamer); ok {
		route = n.RouteName()
	}
	if heavy.due(route, time.Now()) {
		tm.log.Warn("heavy route: move its CPU work to offload()", "route", route, "longest_run_ms", run.Milliseconds())
	}
}
