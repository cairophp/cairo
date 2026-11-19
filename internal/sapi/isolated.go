package sapi

import (
	"context"
	"runtime"
	"sync/atomic"
)

// In isolated mode a request thread runs one request at a time, so requests
// wait in Go for a free thread, all in one queue, and a thread gets the next
// one only once it has answered. Queuing each request on a thread at once
// (as fiber mode does) leaves it behind that thread's slow requests while
// other threads sit idle: Laravel's p99 at 256 connections was 1.8-3.5 s,
// and is 0.6 s this way. Letting a thread hold one more request gave back
// the 8% of hello-world throughput this costs (the thread waits while its
// answer goes back through Go), but put that request behind a slow one.
//
// idleSlots holds the slot numbers of free threads. Go's channels hand
// values to waiting receivers in the order they started waiting, which
// makes the queue first in, first out.
type idleSlots chan int

func newIdleSlots(n int) idleSlots {
	s := make(idleSlots, n)
	for i := 0; i < n; i++ {
		s <- i
	}
	return s
}

// pickWorker returns the request thread to run a request on, with sendMu
// held for reading as acquire does, and release extended to give the
// thread back. In isolated mode it waits for a free thread, or for ctx.
func (e *Engine) pickWorker(ctx context.Context, release func()) (*thread, func(), error) {
	if e.idle == nil {
		return acquire(e.workers), release, nil
	}
	select {
	case i := <-e.idle:
		var freed atomic.Bool
		return acquireSlot(e.workers, i), func() {
			if freed.CompareAndSwap(false, true) {
				e.idle <- i
			}
			release()
		}, nil
	case <-ctx.Done():
		release()
		return nil, nil, ctx.Err()
	}
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
