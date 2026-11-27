package core

/*
#include "isolated.h"
#include "loop.h"
*/
import "C"

import (
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"
)

// In isolated mode a request thread runs one request at a time, so requests
// wait for a free thread, all in one queue, and a thread gets the next one
// only once it has answered. Queuing each request on a thread at once (as
// fiber mode does) leaves it behind that thread's slow requests while other
// threads sit idle: Laravel's p99 at 256 connections was 1.8-3.5 s, and is
// 0.6 s this way.
//
// The queue and the free threads are in C (isolated.c), where the thread
// that answers takes the next request in the same call: through Go, the
// answer went to the collector, which freed the slot for a goroutine that
// waited for one and then woke the thread, and the thread had gone to sleep
// meanwhile. That trip was 13% of a real request's CPU time. Free slots are
// taken first in, first out, as requests are.
//
// A slot is a place in the pool, not a thread: recycling and reloads give it
// to the replacement (swap), and a request taken for a slot goes to the
// thread that has it then (acquireSlot).

// queuedCalls holds the calls whose events wait in C's queue, by id. C has
// their events; Go has what waits for the answer.
var queuedCalls sync.Map

// startIsolated readies C's queue for n request threads; 0 in fiber mode.
func startIsolated(n int) error {
	if C.cairo_isolated_start(C.int(n)) != 0 {
		return errNoMemory
	}
	return nil
}

// placeIsolated tells C which slot thread t answers for, before it starts.
func placeIsolated(t *thread) {
	slot := -1
	if t.pool == poolWorker {
		slot = t.slot
	}
	C.cairo_isolated_place(C.int(t.id), C.int(slot))
}

// queueIsolated gives call c, whose event is data with tail behind it, to a
// free request thread, or else queues it until one is free. c.thread is set
// once a thread has it (c.placed).
func (e *Engine) queueIsolated(c *call, data, tail []byte) error {
	c.queued.Store(c.id)
	queuedCalls.Store(c.id, c)
	s := C.cairo_isolated_dispatch(C.int64_t(c.id), (*C.char)(unsafe.Pointer(unsafe.SliceData(data))), C.size_t(len(data)),
		(*C.char)(unsafe.Pointer(unsafe.SliceData(tail))), C.size_t(len(tail)))
	if s == -1 {
		return nil // a thread that answers takes it (goIsolatedTake)
	}
	queuedCalls.Delete(c.id)
	c.queued.Store(0)
	if s < -1 {
		return errNoMemory
	}
	t := acquireSlot(e.workers, int(s))
	c.thread = t
	t.inflight.Add(1)
	if err := t.begin(c, data, tail); err != nil {
		t.inflight.Add(-1)
		C.cairo_isolated_release(s)
		return err
	}
	c.placed.Store(true)
	return nil
}

// unqueue takes call c, whose context ended, out of the queue. It reports
// false when a thread has it: c.thread is set then.
func unqueue(c *call) bool {
	if c.queued.CompareAndSwap(c.id, 0) {
		queuedCalls.Delete(c.id)
		return true // its event is left in C's queue, which drops it
	}
	for !c.placed.Load() {
		runtime.Gosched() // a thread is taking it just now
	}
	return false
}

// releaseSlot is the end of the request that held slot, for a thread that
// ended without answering it.
func releaseSlot(slot int) {
	C.cairo_isolated_release(C.int(slot))
}

// goIsolatedTake gives the call of event m, which waited in C's queue, to the
// thread in slot, and reports whether it did: 0 for a call whose context
// ended meanwhile.
//
//export goIsolatedTake
func goIsolatedTake(slot C.int, id C.int64_t, m *C.cairo_msg) C.int {
	v, ok := queuedCalls.Load(int64(id))
	if !ok {
		return 0
	}
	c := v.(*call)
	// The id, not a flag: c may be another request's by now (requests).
	if !c.queued.CompareAndSwap(int64(id), 0) {
		return 0
	}
	queuedCalls.Delete(int64(id))
	t := acquireSlot(engine.workers, int(slot))
	c.thread = t
	t.inflight.Add(1)
	t.mu.Lock()
	t.calls[c.id] = c
	t.mu.Unlock()
	C.cairo_inbox_queue(C.int(t.id), m)
	t.sendMu.RUnlock()
	c.placed.Store(true)
	return 1
}

// acquireSlot returns the thread in slot i, waiting while a recycled one is
// replaced, with sendMu held for reading.
func acquireSlot(ts []atomic.Pointer[thread], i int) *thread {
	for {
		t := ts[i].Load()
		t.sendMu.RLock()
		if !t.retired {
			return t
		}
		t.sendMu.RUnlock()
		runtime.Gosched()
	}
}
