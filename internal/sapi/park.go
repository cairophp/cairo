package sapi

import "C"

import (
	"context"
	"sync"
	"time"
)

// Parking: how a fiber waits on Cairo\Channel and select().
//
// Channels live in PHP, between fibers of one PHP thread, so handing a value
// over needs no Go. But a fiber blocked on a channel must still wake when
// its context ends or select()'s timeout passes, so it suspends through a
// "park" operation: its goroutine waits for the context, the timer, or
// cairo_wake() from the fiber that readied it. Every wake-up then comes back
// as an ordinary I/O event, so PHP resumes fibers in one place only.

var parks sync.Map // park op id -> chan struct{}

// woken stands for a wake that came before the goroutine started to wait.
var woken = make(chan struct{})

func init() { close(woken) }

// park returns "woken", "timeout" or "done" (the context ended).
func (e *Engine) park(ctx context.Context, id int64, timeoutSec float64) string {
	ch := make(chan struct{}, 1)
	if _, loaded := parks.LoadOrStore(id, ch); loaded {
		parks.Delete(id)
		return "woken"
	}
	defer parks.Delete(id)
	var timeout <-chan time.Time
	if timeoutSec >= 0 {
		t := time.NewTimer(time.Duration(timeoutSec * float64(time.Second)))
		defer t.Stop()
		timeout = t.C
	}
	select {
	case <-ch:
		return "woken"
	case <-timeout:
		return "timeout"
	case <-ctx.Done():
		return "done"
	}
}

//export goFiberWake
func goFiberWake(id C.longlong) {
	v, loaded := parks.LoadOrStore(int64(id), woken)
	if !loaded {
		// park() finds the marker when it starts. If it already returned
		// (a timeout racing this wake), drop the marker later.
		time.AfterFunc(time.Minute, func() { parks.CompareAndDelete(int64(id), woken) })
		return
	}
	if ch := v.(chan struct{}); ch != woken {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
