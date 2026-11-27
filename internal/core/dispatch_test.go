package core

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// Busy is running PHP: the thread came to its loop, found work and has not
// come back since. What keeps it is the fiber it is in, which began when the
// thread left its loop or, with several fibers ready, when the one before it
// paused.
func TestEachBusyThread(t *testing.T) {
	since := time.Date(2026, 11, 20, 1, 2, 3, 456, time.UTC)
	waiting, busy := &thread{id: 4, box: &mailbox{}}, &thread{id: 7, box: &mailbox{}}
	busy.box.since.Store(since.UnixNano())
	e := &Engine{workers: make([]atomic.Pointer[thread], 3)}
	e.workers[0].Store(waiting)
	e.workers[2].Store(busy)                  // and no thread in slot 1
	waiting.box.fiber.Store(since.UnixNano()) // of a stretch that is over: the thread sleeps

	for _, tc := range []struct {
		name  string
		fiber time.Time // when the thread last went on to a ready fiber
		want  time.Time
	}{
		{"the first fiber of the stretch", time.Time{}, since},
		{"a fiber of an earlier stretch", since.Add(-time.Second), since},
		{"a later fiber of this stretch", since.Add(30 * time.Millisecond), since.Add(30 * time.Millisecond)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			busy.box.fiber.Store(0)
			if !tc.fiber.IsZero() {
				busy.box.fiber.Store(tc.fiber.UnixNano())
			}
			got := map[int]time.Time{}
			e.EachBusyThread(func(tid int, since time.Time) { got[tid] = since })
			if len(got) != 1 || !got[7].Equal(tc.want) {
				t.Fatalf("busy threads: %v, want only thread 7 since %s", got, tc.want)
			}
		})
	}
}

// A request's objects go back for the next request with nothing of the last
// one in them: no answer, no context, no probe, and no buffer that one large
// request grew.
func TestRequestFree(t *testing.T) {
	r := requests.Get().(*request)
	resp := r.call.resp
	r.call.id, r.call.thread, r.call.ctx = 7, &thread{}, context.Background()
	r.call.reply = reply{meta: []byte("{}"), body: []byte("body")}
	r.call.queued.Store(7)
	r.call.placed.Store(true)
	r.buf = make([]byte, 0, maxEventBuf+1)

	r.free()
	if c := &r.call; c.id != 0 || c.thread != nil || c.ctx != nil || c.probe != nil || c.reply.meta != nil || c.reply.body != nil || c.queued.Load() != 0 || c.placed.Load() {
		t.Errorf("the call kept %+v", &r.call)
	}
	if r.call.resp != resp {
		t.Error("the call's channel was replaced")
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
			asleep := &thread{id: 0, box: &mailbox{}}
			awake := &thread{id: 1, box: &mailbox{}}
			now := time.Now().UnixNano()
			awake.box.since.Store(now - tc.ran)
			awake.box.stretch.Store(tc.stretch)
			awake.inflight.Store(tc.inflight)
			awake.box.pushed.Store(int64(tc.queued))
			ts := make([]atomic.Pointer[thread], 2)
			ts[0].Store(asleep)
			ts[1].Store(awake)
			if got := leastBusy(ts, now); (got == awake) != tc.wantAwake {
				t.Errorf("chose thread %d, want the awake thread: %v", got.id, tc.wantAwake)
			}
		})
	}
}

// A call waits for a thread whose inbox is full, for as long as its context
// lasts, and holds nothing of the thread meanwhile: the thread can still be
// retired. With room, acquire returns the thread held for reading.
func TestAcquireWaitsForRoom(t *testing.T) {
	th := &thread{id: 0, box: &mailbox{}}
	ts := make([]atomic.Pointer[thread], 1)
	ts[0].Store(th)

	got, err := acquire(context.Background(), ts)
	if err != nil || got != th {
		t.Fatalf("acquire: thread %v, error %v", got, err)
	}
	if th.sendMu.TryLock() {
		t.Fatal("acquire returned a thread it does not hold")
	}
	th.sendMu.RUnlock()

	th.box.pushed.Store(inboxSize)
	ctx, cancel := context.WithTimeout(context.Background(), 3*inboxRetry)
	defer cancel()
	if got, err := acquire(ctx, ts); err != context.DeadlineExceeded {
		t.Fatalf("acquire with a full inbox: thread %v, error %v", got, err)
	}
	if !th.sendMu.TryLock() {
		t.Fatal("acquire kept the thread it gave up on")
	}
}
