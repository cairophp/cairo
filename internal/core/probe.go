package core

import (
	"time"

	"github.com/cairophp/cairo/internal/wire"
)

// A probe follows one request through the engine: the waits of its fibers
// in Go and the stretches of PHP its thread ran for it. The engine measures
// nothing itself. An extension that does (the request timing) asks for a
// probe per request with ProbeRequests; when none asks, a request costs one
// nil check at each of these points.
//
// A request's fibers wait on their own goroutines, so WaitBegin and WaitEnd
// are called by several goroutines at once, while the request's PHP thread
// calls Ran.
type Probe interface {
	// WaitBegin marks the start of an operation that one of the request's
	// fibers asked Go for. What it returns goes back to WaitEnd.
	WaitBegin() time.Time
	// WaitEnd marks the end of that operation; op is its name.
	WaitEnd(op string, began time.Time)
	// Ran reports one stretch of PHP: the thread resumed one of the
	// request's fibers and ran for d before it came back to Go.
	Ran(d time.Duration)
	// Done is the end of the request: PHP answered (end is empty), or the
	// request's context ended first (end is CtxError's text). A request
	// that never reached PHP, or whose thread stopped, gets none.
	Done(ev wire.Event, end string)
}

// ProbeRequests has the engine make a probe for every request with fn. Call
// it from a start function (RegisterStart), before the first request.
func (e *Engine) ProbeRequests(fn func() Probe) {
	if e.newProbe != nil {
		panic("core: ProbeRequests: the engine keeps one probe per request, and a second extension asks for one")
	}
	e.newProbe = fn
}
