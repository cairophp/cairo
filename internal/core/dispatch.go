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
// resp receives nil when the thread died before answering.
type call struct {
	thread  *thread
	resp    chan *reply
	started atomic.Bool // PHP took it from the inbox
}

// reply is PHP's answer to a call: its metadata (JSON) and, for an HTTP
// response, the body as raw bytes.
type reply struct {
	meta []byte
	body []byte
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
	ctx, cancel := context.WithCancel(ctx)
	c := &call{thread: t, resp: make(chan *reply, 1)}
	f := &fiber{ctx: ctx, cancel: cancel, thread: t, ops: make(chan op, 1), probe: p}
	id := e.nextID.Add(1)
	f.id = id
	e.calls.Store(id, c)
	e.fibers.Store(id, f)
	t.inflight.Add(1)

	var deadline float64
	if dl, ok := ctx.Deadline(); ok {
		deadline = float64(dl.UnixNano()) / 1e9
	}
	b := ev.AppendWire(make([]byte, 0, 512), id, deadline)
	select {
	case t.inbox <- event{data: b, call: c, probe: p}:
		t.sendMu.RUnlock()
	case <-ctx.Done():
		t.sendMu.RUnlock()
		e.forget(id, c, f)
		release()
		return nil, nil, ctx.Err()
	}
	for {
		select {
		case r := <-c.resp:
			release()
			e.fibers.Delete(id)
			cancel()
			if r == nil {
				return nil, nil, ErrThreadStopped
			}
			if p != nil {
				p.Done(ev, "")
			}
			return r.meta, r.body, nil
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
						e.fibers.Delete(id)
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

func (e *Engine) forget(id int64, c *call, f *fiber) {
	e.calls.Delete(id)
	e.fibers.Delete(id)
	c.thread.inflight.Add(-1)
	f.cancel()
}

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
	if ev.call != nil {
		ev.call.started.Store(true)
		t.served.Add(1)
	}
	*n = C.int(len(ev.data))
	return C.CBytes(ev.data)
}

//export goRespond
func goRespond(id C.longlong, data *C.char, n C.int, body *C.char, bn C.int) {
	e := engine
	v, ok := e.calls.LoadAndDelete(int64(id))
	if !ok {
		return
	}
	c := v.(*call)
	t := c.thread
	t.inflight.Add(-1)
	// The request's probe is done as soon as the answer arrives, before
	// this stretch of PHP ends in goNextEvent: report it up to now.
	if t.running != nil {
		t.running.Ran(time.Duration(time.Now().UnixNano() - t.busySince.Load()))
	}
	r := &reply{meta: C.GoBytes(unsafe.Pointer(data), n)}
	if bn > 0 {
		r.body = C.GoBytes(unsafe.Pointer(body), bn)
	}
	c.resp <- r
}

// Respond is goRespond for an extension whose C code answers the requests
// of its server: cgo exports a Go function to the C code of its own package
// only, so the extension exports one that calls this. meta (what the server
// makes of the answer) and body are C memory, n and bn bytes long, copied
// before Respond returns.
func Respond(id int64, meta unsafe.Pointer, n int, body unsafe.Pointer, bn int) {
	goRespond(C.longlong(id), (*C.char)(meta), C.int(n), (*C.char)(body), C.int(bn))
}
