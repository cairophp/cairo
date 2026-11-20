package sapi

import (
	"time"

	"github.com/cairophp/cairo/internal/wire"
)

// A probe follows one request through the engine: the waits of its fibers
// in Go and the stretches of PHP its thread ran for it. The engine measures
// nothing itself. An extension that does (the request timing) asks for a
// probe per request with probeRequests; when none asks, a request costs one
// nil check at each of these points.
//
// A request's fibers wait on their own goroutines, so waitBegin and waitEnd
// are called by several goroutines at once, while the request's PHP thread
// calls ran.
type probe interface {
	// waitBegin marks the start of an operation that one of the request's
	// fibers asked Go for. What it returns goes back to waitEnd.
	waitBegin() time.Time
	// waitEnd marks the end of that operation; op is its name.
	waitEnd(op string, began time.Time)
	// ran reports one stretch of PHP: the thread resumed one of the
	// request's fibers and ran for d before it came back to Go.
	ran(d time.Duration)
	// done is the end of the request: PHP answered (end is empty), or the
	// request's context ended first (end is ctxError's text). A request
	// that never reached PHP, or whose thread stopped, gets none.
	done(ev wire.Event, end string)
}

// probeRequests has the engine make a probe for every request with fn. Call
// it from a start function (registerStart), before the first request.
func (e *Engine) probeRequests(fn func() probe) {
	if e.newProbe != nil {
		panic("sapi: probeRequests: the engine keeps one probe per request, and a second extension asks for one")
	}
	e.newProbe = fn
}
