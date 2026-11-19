package sapi

import (
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cairophp/cairo/internal/wire"
)

// App::slowRequests(): every wait of a request's fibers already runs in Go
// (runOp), so Go adds up how long each kind of wait took and logs a request
// over the threshold with that breakdown. Off, it costs one nil check per
// operation.

type waitKind uint8

const (
	waitSQL waitKind = iota
	waitHTTP
	waitSocket
	waitConnect
	waitSleep
	waitChannel
	waitOffload
	waitGRPC
	waitOther
	numWaitKinds
)

var waitKindNames = [numWaitKinds]string{"sql", "http", "socket", "connect", "sleep", "wait", "offload", "grpc", "other"}

// opKind sorts a fiber operation (see runOp) into the kind it is logged as.
func opKind(op string) waitKind {
	switch op {
	case "pg":
		return waitSQL
	case "http", "waitset": // waitset: a hooked curl transfer
		return waitHTTP
	case "wait":
		return waitSocket
	case "dial", "lookup", "starttls", "stoptls":
		return waitConnect
	case "sleep":
		return waitSleep
	case "park":
		return waitChannel
	case "offload":
		return waitOffload
	}
	switch {
	case strings.HasPrefix(op, "sql."):
		return waitSQL
	case strings.HasPrefix(op, "grpc."):
		return waitGRPC
	}
	return waitOther
}

// reqTiming is one request's waits, added by all its fibers at once.
type reqTiming struct {
	start time.Time
	count [numWaitKinds]atomic.Int64
	nanos [numWaitKinds]atomic.Int64

	// Time with at least one of the request's fibers waiting in Go. The
	// rest is PHP: running, or queued behind other fibers on the thread.
	mu      sync.Mutex
	waiting int
	since   time.Time
	covered time.Duration

	// The longest stretch its thread ran PHP without coming back to Go,
	// starting with one of this request's fibers: while it ran, every other
	// request on the thread waited.
	longest atomic.Int64
}

// ran records one stretch of PHP that resumed this request.
func (t *reqTiming) ran(d time.Duration) {
	for {
		old := t.longest.Load()
		if int64(d) <= old || t.longest.CompareAndSwap(old, int64(d)) {
			return
		}
	}
}

// begin marks the start of a wait and returns its start time.
func (t *reqTiming) begin() time.Time {
	now := time.Now()
	t.mu.Lock()
	if t.waiting == 0 {
		t.since = now
	}
	t.waiting++
	t.mu.Unlock()
	return now
}

// end records one wait of kind k that began at start.
func (t *reqTiming) end(k waitKind, start time.Time) {
	now := time.Now()
	t.count[k].Add(1)
	t.nanos[k].Add(int64(now.Sub(start)))
	t.mu.Lock()
	if t.waiting--; t.waiting == 0 {
		t.covered += now.Sub(t.since)
	}
	t.mu.Unlock()
}

// phpTime is the request's time so far with none of its fibers waiting.
func (t *reqTiming) phpTime(now time.Time) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	covered := t.covered
	if t.waiting > 0 {
		covered += now.Sub(t.since)
	}
	return now.Sub(t.start) - covered
}

// routeNamer is an event that knows its route ("GET /users/{id}").
type routeNamer interface {
	RouteName() string
}

// logSlow logs the request if it took longer than the threshold. end is
// set when the request ended without PHP's answer: deadline_exceeded, or
// canceled when the client went away.
func (e *Engine) logSlow(ev wire.Event, t *reqTiming, end string) {
	now := time.Now()
	total := now.Sub(t.start)
	if e.slowAfter == 0 || total < e.slowAfter {
		return
	}
	attrs := make([]any, 0, 2*(numWaitKinds+4))
	if n, ok := ev.(routeNamer); ok {
		attrs = append(attrs, "route", n.RouteName())
	}
	attrs = append(attrs, "ms", total.Milliseconds(), "php_ms", t.phpTime(now).Milliseconds(),
		"longest_run_ms", time.Duration(t.longest.Load()).Milliseconds())
	for k := range numWaitKinds {
		if n := t.count[k].Load(); n > 0 {
			ms := time.Duration(t.nanos[k].Load()).Milliseconds()
			attrs = append(attrs, waitKindNames[k], strconv.FormatInt(n, 10)+" in "+strconv.FormatInt(ms, 10)+"ms")
		}
	}
	if end != "" {
		attrs = append(attrs, "ended", end)
	}
	e.log.Warn("slow request", attrs...)
}
