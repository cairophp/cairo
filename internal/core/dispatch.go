package core

/*
#include <stdlib.h>
#include "cairo.h"
*/
import "C"

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

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
	// The request's context and probe, which the operations of its fibers
	// run under and report to (submit).
	ctx   context.Context
	probe Probe
	reply reply
	// release gives back what the call holds of the engine (its thread's
	// slot in isolated mode): the collector calls it as the answer comes,
	// so that the next request does not wait for this one's goroutine.
	// Calling it again does nothing.
	release func()
	// In isolated mode, the call's id while its event waits in C's queue
	// (0 once a thread takes it, or it left), and whether it has its thread.
	queued atomic.Int64
	placed atomic.Bool
}

// reply is PHP's answer to a call: its metadata (JSON) and, for an HTTP
// response, the body as raw bytes. A large body is not copied for it: it is
// the memory of the message it came in (lent), which whoever takes the reply
// frees.
type reply struct {
	meta []byte
	body []byte
	lent unsafe.Pointer
}

// lendMin is the size from which a body stays in C's memory. Go gives every
// allocation past 32 KiB spans of its own, under the heap's lock, and a copy
// of that size is no longer small beside what else a request costs.
const lendMin = 32 << 10

// free frees the body that the reply was lent, if any. Nothing may read the
// body afterwards.
func (a *reply) free() {
	if a.lent != nil {
		C.free(a.lent)
		a.lent = nil
	}
}

// request is what Dispatch needs for one request and keeps for the next:
// the call with its channel, and the event's buffer.
type request struct {
	call call
	buf  []byte
}

var requests = sync.Pool{New: func() any {
	r := &request{buf: make([]byte, 0, eventBuf)}
	r.call.resp = make(chan *reply, 1)
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
// call from the thread's table first.
func (r *request) free() {
	r.call = call{resp: r.call.resp}
	if cap(r.buf) > maxEventBuf {
		r.buf = make([]byte, 0, eventBuf)
	}
	requests.Put(r)
}

// Dispatch runs one request on a PHP request thread and returns PHP's answer:
// its metadata (JSON) and, for HTTP, the response body.
//
// ev carries the "type" that the server's PHP file answers
// (Scheduler::answerWith()); Dispatch gives it the "id" and "deadline". The
// calling goroutine only waits: what the request's fibers ask Go for runs in
// goroutines of its own, under ctx. If ctx ends first, Dispatch returns
// ctx.Err(), and a helper goroutine waits for PHP to finish.
//
// ctx must end once the caller is done with the request, as a server's
// request context does: that is what ends the waits of a fiber the request
// left running (an async() that nobody awaited).
func (e *Engine) Dispatch(ctx context.Context, ev wire.Event) (meta, body []byte, err error) {
	a, err := e.dispatch(ctx, ev)
	if err != nil {
		return nil, nil, err
	}
	if a.lent != nil {
		a.body = bytes.Clone(a.body)
		a.free()
	}
	return a.meta, a.body, nil
}

// DispatchLent is Dispatch for a caller that only passes the body on, as a
// server does that writes it to its client. A large body is then lent and
// not copied: when release is not nil, body is memory of C that release
// frees. The caller calls release when it is done with body, and keeps no
// part of body after that. Calling release again does nothing.
func (e *Engine) DispatchLent(ctx context.Context, ev wire.Event) (meta, body []byte, release func(), err error) {
	a, err := e.dispatch(ctx, ev)
	if err != nil {
		return nil, nil, nil, err
	}
	if a.lent == nil {
		return a.meta, a.body, nil, nil
	}
	l := &lentBody{msg: a.lent}
	return a.meta, a.body, l.release, nil
}

// lentBody is the message a lent body is in, until it is released.
type lentBody struct{ msg unsafe.Pointer }

func (l *lentBody) release() {
	if p := atomic.SwapPointer(&l.msg, nil); p != nil {
		C.free(p)
	}
}

// dispatch is Dispatch with the answer as PHP's thread sent it: the caller
// frees it.
func (e *Engine) dispatch(ctx context.Context, ev wire.Event) (reply, error) {
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
			return reply{}, ctx.Err()
		}
	}
	// The event is encoded before the wait for a thread (isolated mode), so
	// that a thread that frees up gets it at once.
	id := e.nextID.Add(1)
	r := requests.Get().(*request)
	var deadline float64
	if dl, ok := ctx.Deadline(); ok {
		deadline = float64(dl.UnixNano()) / 1e9
	}
	r.buf = ev.AppendWire(r.buf[:0], id, deadline)
	var tail []byte
	if te, ok := ev.(wire.TailEvent); ok {
		tail = te.Tail()
	}
	c := &r.call
	c.id, c.ctx, c.probe, c.release = id, ctx, p, release
	if err := e.place(ctx, c, r.buf, tail); err != nil {
		release()
		r.free()
		return reply{}, err
	}
	select {
	case answer := <-c.resp:
		release()
		if answer == nil {
			r.free()
			return reply{}, ErrThreadStopped
		}
		a := *answer
		r.free()
		if p != nil {
			p.Done(ev, "")
		}
		return a, nil
	case <-ctx.Done():
		if e.isolated && unqueue(c) {
			release()
			r.free()
			if p != nil {
				p.Done(ev, CtxError(ctx))
			}
			return reply{}, ctx.Err()
		}
		go e.abandon(ctx, r, release)
		if p != nil {
			p.Done(ev, CtxError(ctx))
		}
		return reply{}, ctx.Err()
	}
}

// place gives call c, whose event is data with tail behind it, to a request
// thread.
func (e *Engine) place(ctx context.Context, c *call, data, tail []byte) error {
	if e.isolated {
		return e.queueIsolated(c, data, tail)
	}
	t, err := acquire(ctx, e.workers)
	if err != nil {
		return err
	}
	c.thread = t
	t.inflight.Add(1)
	if err := t.begin(c, data, tail); err != nil {
		t.inflight.Add(-1)
		return err
	}
	return nil
}

// abandon sees a request through whose context ended before PHP answered:
// Dispatch has returned, and PHP still runs. The operations of its fibers
// end with ctx by themselves; the fibers that wait on the thread itself (a
// sleep, a channel) have no goroutine that sees it end, so PHP ends them.
// Past the deadline it also interrupts the thread until PHP answers: PHP
// code that never waits is stopped where it runs.
func (e *Engine) abandon(ctx context.Context, r *request, release func()) {
	c := &r.call
	t := c.thread
	e.post(t, map[string]any{"type": "cancel", "id": c.id, "error": CtxError(ctx)})
	var tick <-chan time.Time
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		C.cairo_interrupt(C.int(t.id))
		ticker := time.NewTicker(interruptEvery)
		defer ticker.Stop()
		tick = ticker.C
	}
	for {
		select {
		case a := <-c.resp:
			if a != nil {
				a.free() // nobody waits for the answer any more
			}
			release() // the slot is free once PHP is done with the request
			r.free()
			return
		case <-tick:
			C.cairo_interrupt(C.int(t.id))
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

// CallContext is the context of call id while PHP thread tid holds it, the
// one its operations run under: for what an extension does in Go for the
// call when the thread itself calls in, with no operation (a request served
// in C, which waits with its thread). Nil once the call is answered, and for
// a call of another thread.
func CallContext(tid int, id int64) context.Context {
	t := engine.own(C.int(tid))
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if c := t.calls[id]; c != nil {
		return c.ctx
	}
	return nil
}

// EachBusyThread calls fn for every request thread that is running PHP, with
// since when it runs the fiber it is in: since it left its loop, or since it
// went on to that fiber from another that was ready with it.
func (e *Engine) EachBusyThread(fn func(tid int, since time.Time)) {
	for i := range e.workers {
		t := e.workers[i].Load()
		if t == nil {
			continue
		}
		if since := t.box.since.Load(); since != 0 {
			fn(t.id, time.Unix(0, max(since, t.box.fiber.Load())))
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
// holds as acquire returned it, and lets go of the thread. The call is in
// the thread's table before the thread can answer it or ask for it. The
// event is data with tail behind it (wire.TailEvent).
func (t *thread) begin(c *call, data, tail []byte) error {
	defer t.sendMu.RUnlock()
	t.mu.Lock()
	t.calls[c.id] = c
	t.mu.Unlock()
	err := t.pushCall(c.id, data, tail)
	if err != nil {
		t.mu.Lock()
		delete(t.calls, c.id)
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
