package core

/*
#include <stdlib.h>
#include "cairo.h"
*/
import "C"

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cairophp/cairo/internal/wire"
)

// call is one unit of work sent to a PHP thread, waiting for its answer
// (cairo_answer() in extension.h).
// resp receives the call's own reply, or nil when the thread died before
// answering.
type call struct {
	id     int64
	thread *thread
	resp   chan *reply
	fiber  *fiber // a request's root fiber; nil for an offload job
	reply  reply
}

// reply is PHP's answer to a call: its metadata (JSON) and, for an HTTP
// response, the body as raw bytes.
type reply struct {
	meta []byte
	body []byte
}

// request is what Dispatch needs for one request and keeps for the next:
// the call and the root fiber with their channels, and the event's buffer.
type request struct {
	call  call
	fiber fiber
	buf   []byte
}

var requests = sync.Pool{New: func() any {
	r := &request{buf: make([]byte, 0, eventBuf)}
	r.call.resp = make(chan *reply, 1)
	r.call.fiber = &r.fiber
	r.fiber.ops = make(chan op, 1)
	return r
}}

// An event's buffer starts at eventBuf bytes, and one that a large request
// grew past maxEventBuf is not kept.
const (
	eventBuf    = 512
	maxEventBuf = 64 << 10
)

// free gives r back for another request. Nothing may hold it any more: its
// event was never queued, or its thread answered the call, which drops the
// call and the fiber from the thread's tables first.
func (r *request) free() {
	select {
	case <-r.fiber.ops: // sent as PHP answered: its fiber ended with the request
	default:
	}
	r.call.thread, r.call.reply = nil, reply{}
	r.fiber = fiber{ops: r.fiber.ops}
	if cap(r.buf) > maxEventBuf {
		r.buf = make([]byte, 0, eventBuf)
	}
	requests.Put(r)
}

// Dispatch runs one request on a PHP request thread and returns PHP's answer:
// its metadata (JSON) and, for HTTP, the response body.
//
// The calling goroutine becomes the request's root fiber: until PHP answers,
// it performs that fiber's I/O under ctx. ev carries the "type" that the
// server's PHP file answers (Scheduler::answerWith()); Dispatch gives it the
// "id" and "deadline". If ctx ends first, Dispatch returns ctx.Err() and a
// helper goroutine keeps answering the fiber (with the context's error)
// until PHP finishes.
//
// The request's fibers wait under ctx itself, so ctx must end once the
// caller is done with the request, as a server's request context does: that
// is what ends the waits of a fiber the request left running (an async()
// that nobody awaited).
func (e *Engine) Dispatch(ctx context.Context, ev wire.Event) (meta, body []byte, err error) {
	var p Probe
	if e.newProbe != nil {
		p = e.newProbe()
	}
	release := func() {}
	if e.slots != nil {
		// Backpressure: wait here, in Go, for a free slot.
		select {
		case e.slots <- struct{}{}:
			var once sync.Once
			release = func() { once.Do(func() { <-e.slots }) }
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
	t, release, err := e.pickWorker(ctx, release)
	if err != nil {
		return nil, nil, err
	}
	id := e.nextID.Add(1)
	r := requests.Get().(*request)
	c, f := &r.call, &r.fiber
	c.id, c.thread = id, t
	*f = fiber{id: id, ctx: ctx, thread: t, ops: f.ops, probe: p}
	t.inflight.Add(1)

	var deadline float64
	if dl, ok := ctx.Deadline(); ok {
		deadline = float64(dl.UnixNano()) / 1e9
	}
	r.buf = ev.AppendWire(r.buf[:0], id, deadline)
	if err := t.begin(c, r.buf); err != nil {
		t.inflight.Add(-1)
		release()
		r.free()
		return nil, nil, err
	}
	for {
		select {
		case answer := <-c.resp:
			release()
			if answer == nil {
				r.free()
				return nil, nil, ErrThreadStopped
			}
			meta, body = answer.meta, answer.body
			r.free()
			if p != nil {
				p.Done(ev, "")
			}
			return meta, body, nil
		case o := <-f.ops:
			e.runOp(f, o)
		case <-ctx.Done():
			// Past the deadline, interrupt the thread until PHP answers: PHP
			// code that never waits on Go is stopped where it runs.
			deadline := errors.Is(ctx.Err(), context.DeadlineExceeded)
			go func() {
				// The fibers that wait on the thread itself (a sleep, a
				// channel) have no goroutine that sees ctx end: PHP ends them.
				e.post(t, map[string]any{"type": "cancel", "id": id, "error": CtxError(ctx)})
				var tick <-chan time.Time
				if deadline {
					C.cairo_interrupt(C.int(t.id))
					ticker := time.NewTicker(interruptEvery)
					defer ticker.Stop()
					tick = ticker.C
				}
				for {
					select {
					case <-c.resp:
						release() // the slot is free once PHP is done with the request
						r.free()
						return
					case o := <-f.ops:
						e.runOp(f, o)
					case <-tick:
						C.cairo_interrupt(C.int(t.id))
					}
				}
			}()
			if p != nil {
				p.Done(ev, CtxError(ctx))
			}
			return nil, nil, ctx.Err()
		}
	}
}

// interruptEvery is how often a request past its deadline interrupts its
// PHP thread until PHP answers.
const interruptEvery = 50 * time.Millisecond

// stuckAfter is how long a thread's PHP may run without coming back for an
// event before dispatch passes it over: it is running one fiber that does
// not pause (a CPU loop, an unhooked blocking call), and everything queued
// there waits for it.
const stuckAfter = 5 * time.Millisecond

// awakeWait is how long a call may wait for a thread that is running PHP
// while another thread sleeps. Waking a thread costs about 10 µs on the
// waker's core and on its own, and a thread that comes back for its next
// event anyway takes the call for nothing. Measured on a route that answers
// at once: 20 µs gains about half of what 50 µs does, and 200 µs little
// more, for a wait four times as long when a core is idle.
const awakeWait = 50 * time.Microsecond

// awakeLead is how many more calls in flight than the least busy thread a
// thread may hold and still be chosen for being awake, so that one thread
// does not end up with every request that waits (and all their memory).
const awakeLead = 32

// leastBusy returns the thread with the fewest calls in flight, among those
// not stuck in one fiber; when every thread is stuck, among all of them. A
// thread that is running PHP and will be back within awakeWait goes first.
// now is the time in unix nanoseconds.
func leastBusy(ts []atomic.Pointer[thread], now int64) *thread {
	stuck := now - int64(stuckAfter)
	var best, free, awake *thread
	var min, freeMin, awakeMin int64
	for i := range ts {
		t := ts[i].Load()
		n := t.inflight.Load()
		since := t.box.since.Load()
		if best == nil || n < min {
			best, min = t, n
		}
		if (since == 0 || since > stuck) && (free == nil || n < freeMin) {
			free, freeMin = t, n
		}
		if since != 0 && t.backSoon(now-since) && (awake == nil || n < awakeMin) {
			awake, awakeMin = t, n
		}
	}
	if awake != nil && awakeMin <= freeMin+awakeLead {
		return awake
	}
	if free != nil {
		return free
	}
	return best
}

// backSoon reports whether a thread that has run PHP for ran nanoseconds
// since its last event is expected to have taken a call queued now within
// awakeWait: it is early in a stretch of PHP, and its stretches are short
// enough for what its inbox already holds.
func (t *thread) backSoon(ran int64) bool {
	return ran < int64(awakeWait) && (t.queued()+1)*t.box.stretch.Load() < int64(awakeWait)
}

// EachBusyThread calls fn for every request thread that is running PHP, with
// when it took the event it is running.
func (e *Engine) EachBusyThread(fn func(tid int, since time.Time)) {
	for i := range e.workers {
		t := e.workers[i].Load()
		if t == nil {
			continue
		}
		if since := t.box.since.Load(); since != 0 {
			fn(t.id, time.Unix(0, since))
		}
	}
}

// InterruptThread has request thread tid run the interrupt of every
// extension (cairo_extension in extension.h) at its next safe point.
func (e *Engine) InterruptThread(tid int) {
	C.cairo_vm_interrupt(C.int(tid))
}

// acquire picks the least busy thread that still takes work and returns it
// with sendMu held for reading: the caller queues its event, then unlocks
// (begin). While that thread's inbox is full, it waits, holding nothing,
// for as long as ctx lasts.
func acquire(ctx context.Context, ts []atomic.Pointer[thread]) (*thread, error) {
	for {
		t := leastBusy(ts, time.Now().UnixNano())
		t.sendMu.RLock()
		full := t.queued() >= inboxSize
		if !t.retired && !full {
			return t, nil
		}
		retired := t.retired
		t.sendMu.RUnlock()
		if retired {
			runtime.Gosched() // its replacement is being put in place
			continue
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(inboxRetry):
		}
	}
}

// begin queues the event that begins call c on thread t, which the caller
// holds as acquire returned it, and lets go of the thread. The call and its
// request's fiber are in the thread's tables before the thread can answer
// or ask for either.
func (t *thread) begin(c *call, data []byte) error {
	defer t.sendMu.RUnlock()
	t.mu.Lock()
	t.calls[c.id] = c
	if c.fiber != nil {
		t.fibers[c.id] = c.fiber
	}
	t.mu.Unlock()
	err := t.push(eventCall, c.id, data)
	if err != nil {
		t.mu.Lock()
		delete(t.calls, c.id)
		delete(t.fibers, c.id)
		t.mu.Unlock()
	}
	return err
}

// post queues an event that begins no call for thread t.
func (e *Engine) post(t *thread, ev any) {
	if err := t.send(eventOther, 0, wire.Encode(ev)); err != nil {
		e.log.Error("event not sent to its php thread", "thread", t.id, "err", err)
	}
}
