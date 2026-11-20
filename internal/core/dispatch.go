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
	"unsafe"

	"github.com/cairophp/cairo/internal/wire"
)

// event is one message for a PHP thread. call is set for a request or an
// offload job, so Go knows when PHP has taken it.
type event struct {
	data  []byte
	call  *call
	probe Probe // of the request it resumes, when requests are probed
}

// call is one unit of work sent to a PHP thread, waiting for cairo_respond().
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
	select {
	case t.inbox <- event{data: r.buf, call: c, probe: p}:
		t.sendMu.RUnlock()
	case <-ctx.Done():
		t.sendMu.RUnlock()
		t.inflight.Add(-1)
		release()
		r.free()
		return nil, nil, ctx.Err()
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

// leastBusy returns the thread with the fewest calls in flight, among those
// not stuck in one fiber; when every thread is stuck, among all of them.
func leastBusy(ts []atomic.Pointer[thread]) *thread {
	stuck := time.Now().UnixNano() - int64(stuckAfter)
	var best, free *thread
	var min, freeMin int64
	for i := range ts {
		t := ts[i].Load()
		n := t.inflight.Load()
		if best == nil || n < min {
			best, min = t, n
		}
		if since := t.busySince.Load(); (since == 0 || since > stuck) && (free == nil || n < freeMin) {
			free, freeMin = t, n
		}
	}
	if free != nil {
		return free
	}
	return best
}

// EachBusyThread calls fn for every request thread that is running PHP, with
// when it took the event it is running.
func (e *Engine) EachBusyThread(fn func(tid int, since time.Time)) {
	for i := range e.workers {
		t := e.workers[i].Load()
		if t == nil {
			continue
		}
		if since := t.busySince.Load(); since != 0 {
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
// with sendMu held for reading: the caller queues its event, then unlocks.
func acquire(ts []atomic.Pointer[thread]) *thread {
	for {
		t := leastBusy(ts)
		t.sendMu.RLock()
		if !t.retired {
			return t
		}
		t.sendMu.RUnlock()
		runtime.Gosched() // its replacement is being put in place
	}
}

func (e *Engine) post(t *thread, ev any) {
	t.inbox <- event{data: wire.Encode(ev)}
}

// goNextEvent returns the thread's next event (internal/wire) in C memory,
// which the caller frees, and its length in n.
//
//export goNextEvent
func goNextEvent(tid C.int, n *C.int) unsafe.Pointer {
	t := engine.table[int(tid)].Load()
	if t.running != nil {
		t.running.Ran(time.Duration(time.Now().UnixNano() - t.busySince.Load()))
	}
	t.busySince.Store(0)
	ev := <-t.inbox
	t.running = ev.probe
	t.busySince.Store(time.Now().UnixNano())
	if c := ev.call; c != nil {
		// From here the thread answers for the call if it dies (failCalls),
		// and its PHP finds the request's fiber.
		t.calls[c.id] = c
		if c.fiber != nil {
			t.fibers[c.id] = c.fiber
		}
		t.served.Add(1)
	}
	*n = C.int(len(ev.data))
	return C.CBytes(ev.data)
}

//export goRespond
func goRespond(tid C.int, id C.longlong, data *C.char, n C.int, body *C.char, bn C.int) {
	t := engine.own(tid)
	if t == nil {
		return
	}
	c, ok := t.calls[int64(id)]
	if !ok {
		return
	}
	delete(t.calls, c.id)
	delete(t.fibers, c.id)
	t.inflight.Add(-1)
	// The request's probe is done as soon as the answer arrives, before
	// this stretch of PHP ends in goNextEvent: report it up to now.
	if t.running != nil {
		t.running.Ran(time.Duration(time.Now().UnixNano() - t.busySince.Load()))
	}
	c.reply.meta = C.GoBytes(unsafe.Pointer(data), n)
	if bn > 0 {
		c.reply.body = C.GoBytes(unsafe.Pointer(body), bn)
	}
	c.resp <- &c.reply
}

// Respond is goRespond for an extension whose C code answers the requests
// of its server: cgo exports a Go function to the C code of its own package
// only, so the extension exports one that calls this. tid is the PHP thread
// that answers (cairo_thread_id() in extension.h). meta (what the server
// makes of the answer) and body are C memory, n and bn bytes long, copied
// before Respond returns.
func Respond(tid int, id int64, meta unsafe.Pointer, n int, body unsafe.Pointer, bn int) {
	goRespond(C.int(tid), C.longlong(id), (*C.char)(meta), C.int(n), (*C.char)(body), C.int(bn))
}
