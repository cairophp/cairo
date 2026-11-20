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

	id := e.submit(0, f.id, op{Op: "sleep"})
	if id <= 0 || len(f.ops) != 1 || (<-f.ops).ID != int64(id) {
		t.Fatalf("the fiber's own thread: operation %d, %d queued", id, len(f.ops))
	}
	if e.fiber(1, f.id) != nil || e.fiber(-1, f.id) != nil || e.fiber(maxThreads, f.id) != nil {
		t.Error("a caller that is not thread 0 found its fiber")
	}
}
