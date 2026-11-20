package core

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// Busy is running PHP: the thread took an event and has not come back for
// the next one.
func TestEachBusyThread(t *testing.T) {
	since := time.Date(2026, 11, 20, 1, 2, 3, 456, time.UTC)
	waiting, busy := &thread{id: 4}, &thread{id: 7}
	busy.busySince.Store(since.UnixNano())
	e := &Engine{workers: make([]atomic.Pointer[thread], 3)}
	e.workers[0].Store(waiting)
	e.workers[2].Store(busy) // and no thread in slot 1

	got := map[int]time.Time{}
	e.EachBusyThread(func(tid int, since time.Time) { got[tid] = since })
	if len(got) != 1 || !got[7].Equal(since) {
		t.Fatalf("busy threads: %v, want only thread 7 since %s", got, since)
	}
}

// A request's objects go back for the next request with nothing of the last
// one in them: no answer, no context, no operation left in the channel, and
// no buffer that one large request grew.
func TestRequestFree(t *testing.T) {
	r := requests.Get().(*request)
	ops, resp := r.fiber.ops, r.call.resp
	r.call.thread = &thread{}
	r.call.reply = reply{meta: []byte("{}"), body: []byte("body")}
	r.fiber = fiber{id: 7, ctx: context.Background(), ops: ops}
	r.fiber.ops <- op{Op: "sleep"}
	r.buf = make([]byte, 0, maxEventBuf+1)

	r.free()
	if r.call.thread != nil || r.call.reply.meta != nil || r.call.reply.body != nil {
		t.Errorf("the call kept %+v", r.call)
	}
	if r.fiber.id != 0 || r.fiber.ctx != nil || len(r.fiber.ops) != 0 {
		t.Errorf("the fiber kept id %d, context %v, %d operations", r.fiber.id, r.fiber.ctx, len(r.fiber.ops))
	}
	if r.fiber.ops != ops || r.call.resp != resp || r.call.fiber != &r.fiber {
		t.Error("the channels or the call's fiber were replaced")
	}
	if cap(r.buf) != eventBuf {
		t.Errorf("an event buffer of %d bytes was kept", cap(r.buf))
	}
}

// A thread that is running PHP and will be back in a moment takes the next
// call, so that no sleeping thread is woken for it. Not when its PHP has
// run for long, when its stretches of PHP are long, when what already
// queues for it takes too long, or when it holds far more calls than the
// least busy thread.
func TestLeastBusyPrefersAwakeThread(t *testing.T) {
	const us = int64(time.Microsecond)
	for _, tc := range []struct {
		name         string
		ran, stretch int64 // of the thread that is awake
		queued       int
		inflight     int64
		wantAwake    bool
	}{
		{"just took an event", 2 * us, 5 * us, 0, 3, true},
		{"calls queued, short stretches", 2 * us, 5 * us, 4, 3, true},
		{"long in one stretch", 80 * us, 5 * us, 0, 3, false},
		{"long stretches", 2 * us, 300 * us, 0, 3, false},
		{"calls queued", 2 * us, 15 * us, 4, 3, false},
		{"holds far more calls", 2 * us, 5 * us, 0, awakeLead + 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asleep := &thread{id: 0}
			awake := &thread{id: 1, inbox: make(chan event, 8)}
			now := time.Now().UnixNano()
			awake.busySince.Store(now - tc.ran)
			awake.stretch.Store(tc.stretch)
			awake.inflight.Store(tc.inflight)
			for range tc.queued {
				awake.inbox <- event{}
			}
			ts := make([]atomic.Pointer[thread], 2)
			ts[0].Store(asleep)
			ts[1].Store(awake)
			if got := leastBusy(ts, now); (got == awake) != tc.wantAwake {
				t.Errorf("chose thread %d, want the awake thread: %v", got.id, tc.wantAwake)
			}
		})
	}
}
