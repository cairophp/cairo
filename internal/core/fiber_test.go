package core

import "testing"

// What a PHP thread asks for is looked up in its own table, which has no
// lock: a fiber of another thread, or a caller that is no PHP thread of the
// pools, must find nothing.
func TestSubmitToOwnFibersOnly(t *testing.T) {
	e := &Engine{}
	a := e.newThread(0, poolWorker, 0)
	e.newThread(1, poolWorker, 1)
	f := &fiber{id: 7, thread: a, ops: make(chan op, 1)}
	a.fibers[f.id] = f

	id := e.submit(0, f.id, op{Op: "offload"})
	if id <= 0 || len(f.ops) != 1 || (<-f.ops).ID != int64(id) {
		t.Fatalf("the fiber's own thread: operation %d, %d queued", id, len(f.ops))
	}
	// An operation is something a goroutine takes over, so the thread
	// looks at its mailbox before it sleeps; a socket wait is not.
	if a.box.handed != 1 {
		t.Error("an operation did not mark the thread as having handed Go something")
	}
	a.box.handed = 0
	if e.submit(0, f.id, op{Op: "wait"}); a.box.handed != 0 {
		t.Error("a socket wait marked the thread as having handed Go something")
	}
	<-f.ops
	if e.fiber(1, f.id) != nil || e.fiber(-1, f.id) != nil || e.fiber(maxThreads, f.id) != nil {
		t.Error("a caller that is not thread 0 found its fiber")
	}
}
