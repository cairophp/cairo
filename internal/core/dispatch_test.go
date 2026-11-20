package core

import (
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
