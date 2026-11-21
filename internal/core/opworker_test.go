package core

import (
	"context"
	"runtime"
	"testing"
	"time"
)

// A goroutine that finished an operation runs the thread's next one; two
// operations at once take two, and all of them end with the thread.
func TestWorkersWaitForTheNextOperation(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	RegisterOp("test.hold", func(context.Context, []byte) (any, error) {
		started <- struct{}{}
		<-release
		return nil, nil
	})
	t.Cleanup(func() { delete(goOps, "test.hold") })
	e := &Engine{}
	a := e.newThread(0, poolWorker, 0)
	t.Cleanup(func() { e.failCalls(a, true) })
	a.calls[7] = &call{id: 7, thread: a, ctx: context.Background(), resp: make(chan *reply, 1)}
	idle := func(want int) []opWorker {
		t.Helper()
		for end := time.Now().Add(5 * time.Second); ; runtime.Gosched() {
			a.mu.Lock()
			ws := append([]opWorker(nil), a.idle...)
			a.mu.Unlock()
			if len(ws) == want {
				return ws
			}
			if time.Now().After(end) {
				t.Fatalf("%d goroutines wait for an operation, want %d", len(ws), want)
			}
		}
	}
	run := func(n int) {
		for i := 0; i < n; i++ {
			e.submit(0, 7, 0, op{Op: "test.hold"})
		}
		for i := 0; i < n; i++ {
			<-started
		}
		idle(0)
		for i := 0; i < n; i++ {
			release <- struct{}{}
		}
	}

	run(1)
	first := idle(1)[0]
	run(1)
	if idle(1)[0] != first {
		t.Error("the second operation did not run on the goroutine that waited")
	}
	run(2)
	ws := idle(2)

	a.endWorkers()
	idle(0)
	for _, w := range ws {
		if _, open := <-w; open {
			t.Error("a goroutine was left waiting after its thread ended")
		}
	}
	queued := a.queued()
	run(1) // on a goroutine of its own, which does not wait afterwards
	for end := time.Now().Add(5 * time.Second); a.queued() == queued; runtime.Gosched() {
		if time.Now().After(end) {
			t.Fatal("the last result was not queued for the thread")
		}
	}
	idle(0)
}
