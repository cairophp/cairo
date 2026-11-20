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
