package core

import (
	"testing"

	"github.com/cairophp/cairo/internal/wire"
)

// What is sent to a thread waits in its inbox until the thread reads it,
// and the calls among it can be told from the rest. An event for a thread
// that has ended is dropped: its inbox may be another thread's by then.
func TestSendQueuesForTheThread(t *testing.T) {
	e := &Engine{}
	th := e.newThread(7, poolWorker, 0)
	t.Cleanup(func() { e.failCalls(th, true) })
	before := th.queued()

	event := wire.Encode(map[string]string{"type": "cancel"})
	for _, ev := range []struct {
		kind int
		id   int64
	}{{eventCall, 11}, {eventOther, 0}, {eventCall, 12}, {eventOther, 11}} {
		if err := th.send(ev.kind, ev.id, event); err != nil {
			t.Fatal(err)
		}
	}
	if n := th.queued() - before; n != 4 {
		t.Errorf("%d events queued, want 4", n)
	}
	if calls := th.queuedCalls(); len(calls) != 2 || !calls[11] || !calls[12] {
		t.Errorf("queued calls %v, want 11 and 12", calls)
	}

	th.gone = true
	if err := th.send(eventCall, 13, event); err != nil || th.queued()-before != 4 {
		t.Errorf("an event for a thread that has ended: error %v, %d queued; want it dropped", err, th.queued()-before)
	}
}
