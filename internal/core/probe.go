package core

/*
#include "loop.h"
*/
import "C"

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
// The operations of a request's fibers run in goroutines of their own, so
// WaitBegin and WaitEnd are called by several goroutines at once, while the
// request's PHP thread calls Ran.
type Probe interface {
	// WaitBegin marks the start of a wait of one of the request's fibers:
	// an operation it asked Go for, or a wait on its PHP thread ("sleep",
	// "park"). What it returns goes back to WaitEnd.
	WaitBegin() time.Time
	// WaitEnd marks the end of that wait; op is its name.
	WaitEnd(op string, began time.Time)
	// Ran reports one stretch of PHP: the thread resumed one of the
	// request's fibers and ran for d before it came back to its loop.
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
	C.cairo_loop_probe()
}

// probeOf is the probe of request id, which the calling PHP thread holds,
// with the thread; a nil probe when the request has none or was answered,
// and a nil thread outside the PHP threads of the pools.
func (e *Engine) probeOf(tid C.int, id int64) (*thread, Probe) {
	t := e.own(tid)
	if t == nil {
		return nil, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if c := t.calls[id]; c != nil {
		return t, c.probe
	}
	return t, nil
}

// goWaitBegan is a fiber of request id that starts a wait on its PHP thread,
// which no goroutine sees: a sleep, or a wait in select(). It returns what
// goWaitEnded takes back, 0 when the request has no probe.
//
//export goWaitBegan
func goWaitBegan(tid C.int, id C.longlong) C.longlong {
	_, p := engine.probeOf(tid, int64(id))
	if p == nil {
		return 0
	}
	return C.longlong(p.WaitBegin().UnixNano())
}

// goWaitEnded is the end of that wait, as the fiber runs again.
//
//export goWaitEnded
func goWaitEnded(tid C.int, id C.longlong, parked C.int, began C.longlong) {
	t, p := engine.probeOf(tid, int64(id))
	if p == nil {
		return
	}
	op := "sleep"
	if parked != 0 {
		op = "park"
	}
	p.WaitEnd(op, time.Unix(0, int64(began)))
	// A stretch of PHP that no event began is its request's.
	if t.running == nil {
		t.running = p
	}
}
