package core

import (
	"context"
	"math"
	"runtime"
	"testing"
	"time"
)

// An operation runs under the context of its request, which only the thread
// that holds the request can name. A scope gives it a context of its own,
// which ends at the scope's deadline or when PHP cancels the operation, and
// which the thread forgets when the operation is over.
func TestSubmitUnderTheRequestAndItsScope(t *testing.T) {
	started, release := make(chan context.Context), make(chan struct{})
	RegisterOp("test.ctx", func(ctx context.Context, _ []byte) (any, error) {
		started <- ctx
		<-release
		return nil, nil
	})
	t.Cleanup(func() { delete(goOps, "test.ctx") })
	e := &Engine{}
	a := e.newThread(0, poolWorker, 0)
	e.newThread(1, poolWorker, 1)
	t.Cleanup(func() { e.failCalls(a, true); a.endWorkers() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.calls[7] = &call{id: 7, thread: a, ctx: ctx, resp: make(chan *reply, 1)}

	if e.submit(1, 7, 0, op{Op: "test.ctx"}) != -1 || e.submit(-1, 7, 0, op{Op: "test.ctx"}) != -1 ||
		e.submit(maxThreads, 7, 0, op{Op: "test.ctx"}) != -1 {
		t.Error("a caller that is not thread 0 started an operation of its request")
	}
	if e.submit(0, 8, 0, op{Op: "test.ctx"}) != -1 {
		t.Error("an operation started for a request the thread does not hold")
	}

	soon := float64(time.Now().Add(20*time.Millisecond).UnixNano()) / 1e9
	for _, tc := range []struct {
		name  string
		scope float64
		end   func(id int64)
		want  error
	}{
		{"a deadline", soon, func(int64) {}, context.DeadlineExceeded},
		{"canceled by PHP", math.Inf(1), a.cancelOp, context.Canceled},
		{"the request's own context", 0, func(int64) { cancel() }, context.Canceled},
	} {
		queued := a.queued()
		id := int64(e.submit(0, 7, tc.scope, op{Op: "test.ctx"}))
		if id <= 0 {
			t.Fatalf("%s: the operation was refused", tc.name)
		}
		got := <-started
		a.mu.Lock()
		_, scoped := a.scoped[id]
		a.mu.Unlock()
		if scoped != (tc.scope != 0) || (got == ctx) != (tc.scope == 0) {
			t.Errorf("%s: a context of its own in the table: %v; the request's context: %v", tc.name, scoped, got == ctx)
		}
		tc.end(id)
		select {
		case <-got.Done():
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the operation's context did not end", tc.name)
		}
		if got.Err() != tc.want {
			t.Errorf("%s: the context ended with %v, want %v", tc.name, got.Err(), tc.want)
		}
		release <- struct{}{}
		// The result is an event for the thread, and the context is gone by then.
		for end := time.Now().Add(5 * time.Second); a.queued() == queued; runtime.Gosched() {
			if time.Now().After(end) {
				t.Fatalf("%s: no result was queued for the thread", tc.name)
			}
		}
		a.mu.Lock()
		left := len(a.scoped)
		a.mu.Unlock()
		if left != 0 {
			t.Errorf("%s: %d contexts left in the thread's table", tc.name, left)
		}
	}
}

// An operation is something a goroutine takes over, so the thread looks at
// its inbox before it sleeps; a socket wait is not: the thread watched the
// socket itself before it asked.
func TestSubmitMakesTheThreadLook(t *testing.T) {
	e := &Engine{}
	a := e.newThread(0, poolWorker, 0)
	t.Cleanup(func() { e.failCalls(a, true); a.endWorkers() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()         // the operations end at once
	a.box.handed = 0 // the mailbox is the thread id's, and outlives a test
	a.calls[7] = &call{id: 7, thread: a, ctx: ctx, resp: make(chan *reply, 1)}

	if e.submit(0, 7, 0, op{Op: "wait", Fd: -1}); a.box.handed != 0 {
		t.Error("a socket wait marked the thread as having handed Go something")
	}
	if e.submit(0, 7, 0, op{Op: "no such operation"}); a.box.handed != 1 {
		t.Error("an operation did not mark the thread as having handed Go something")
	}
	a.box.handed = 0
	for end := time.Now().Add(5 * time.Second); a.queued() < 2; runtime.Gosched() {
		if time.Now().After(end) {
			t.Fatal("the two results were not queued for the thread")
		}
	}
}
