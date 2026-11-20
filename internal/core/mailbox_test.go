package core

import (
	"testing"
	"time"
)

// A send marks the inbox, and a thread that finds its inbox empty takes the
// mark off, unless an event came in between. A thread that waits counts as
// asleep, for as long as it was told at most.
func TestThreadSleep(t *testing.T) {
	th := &thread{inbox: make(chan event, 2), box: &mailbox{}}
	var ev event

	th.box.pending.Store(1)
	if th.queued() || th.box.pending.Load() != 0 {
		t.Errorf("an empty inbox: pending %d; want the mark off", th.box.pending.Load())
	}

	th.inbox <- event{} // sent after the thread looked, its mark not yet set
	if !th.queued() || th.box.pending.Load() != 1 {
		t.Error("an event that came as the thread took the mark off is not marked")
	}
	<-th.inbox

	start := time.Now()
	if th.sleep(&ev, 20*time.Millisecond) || time.Since(start) < 20*time.Millisecond || th.box.since.Load() != 0 {
		t.Errorf("a wait of 20 ms on an empty inbox ended after %s, since %d", time.Since(start), th.box.since.Load())
	}

	go th.send(event{data: []byte("x")})
	for _, wait := range []time.Duration{time.Minute, -1} {
		if !th.sleep(&ev, wait) || string(ev.data) != "x" {
			t.Fatalf("a wait of %s did not end with the event that was sent", wait)
		}
		go th.send(event{data: []byte("x")})
	}
	<-th.inbox
}
