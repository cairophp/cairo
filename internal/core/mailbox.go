package core

/*
#include "loop.h"
*/
import "C"

import (
	"runtime"
	"sync/atomic"
	"time"
	"unsafe"
)

// mailbox is what Go shares with the loop of a PHP thread, which runs in C
// (cairo_mailbox in loop.h, the same memory): the thread reads and writes it
// at every turn of its loop without a call into Go.
type mailbox struct {
	// When the thread last came back to its loop (unix ns; 0 while it
	// sleeps): dispatch passes over a thread that has run PHP for too long
	// (leastBusy), and extensions ask which threads are busy (EachBusyThread).
	since atomic.Int64
	// A moving average of how long the thread runs PHP between two visits
	// to its loop, in nanoseconds: what a call queued behind others waits
	// for each of them.
	stretch atomic.Int64
	// 1 when the thread gave Go an operation or an answer since it last
	// came to its loop: a goroutine has to take that over, and the thread
	// looks before it sleeps (look). Not after a socket wait: the thread
	// watched the socket itself before it asked Go. Only the thread itself
	// reads and writes it, in C and in its calls into Go.
	handed int32
	_      [44]byte // the thread's fields and the senders' share no cache line
	// 1 while the inbox may hold an event: set after every send, cleared by
	// the thread when it finds the inbox empty. A thread whose fibers keep
	// each other busy calls Go for an event only when this says so.
	pending atomic.Int32
	_       [60]byte
}

// The two layouts are one: neither size, and neither place of the mark, may
// exceed the other.
var (
	_ [unsafe.Sizeof(mailbox{}) - uintptr(C.sizeof_cairo_mailbox)]struct{}
	_ [uintptr(C.sizeof_cairo_mailbox) - unsafe.Sizeof(mailbox{})]struct{}
	_ [unsafe.Offsetof(mailbox{}.pending) - uintptr(C.cairo_mailbox_pending_at)]struct{}
	_ [uintptr(C.cairo_mailbox_pending_at) - unsafe.Offsetof(mailbox{}.pending)]struct{}
)

// look is how long a thread watches its mailbox before it sleeps, when it
// handed Go something on its way (look() in loop.c). It has to outlast the wake-up of the Go thread that takes over
// the operation the PHP thread just handed to Go, a few microseconds; what
// it costs is that much CPU for each sleep that it did not spare.
const look = 6 * time.Microsecond

// setLook turns the look on where a second CPU can fill the mailbox
// meanwhile: with one, the thread would only keep it from doing so.
func setLook() {
	if runtime.NumCPU() > 1 {
		C.cairo_loop_look(C.int64_t(look))
	}
}

func mailboxOf(tid int) *mailbox {
	return (*mailbox)(unsafe.Pointer(C.cairo_mailbox_of(C.int(tid))))
}

// send queues ev for the thread. Every send to an inbox is followed by the
// mark that its thread looks at.
func (t *thread) send(ev event) {
	t.inbox <- ev
	t.mark()
}

// mark says that the inbox may hold an event. It writes only what is not
// there yet: senders to a thread with a full inbox then leave its line alone.
func (t *thread) mark() {
	if t.box.pending.Load() == 0 {
		t.box.pending.Store(1)
	}
}

// goNextEvent returns the thread's next event (internal/wire) in C memory,
// which the caller frees, and its length in n. With none queued, it waits
// for one: wait nanoseconds at most (0: not at all; below 0: as long as it
// takes), and returns nil when none came. The thread sleeps on the inbox,
// as a goroutine: Go's scheduler then gives its processor to the goroutines
// that the thread made runnable on its way here.
//
//export goNextEvent
func goNextEvent(tid C.int, wait C.longlong, n *C.int) unsafe.Pointer {
	t := engine.table[int(tid)].Load()
	var ev event
	select {
	case ev = <-t.inbox:
	default:
		// An event that came since is left to the thread's next visit,
		// with the mark on: rare, and the loop is back at once.
		if t.queued() || wait == 0 || !t.sleep(&ev, time.Duration(wait)) {
			return nil
		}
	}
	if len(t.inbox) == 0 {
		t.queued() // the mark comes off with the inbox's last event
	}
	t.running = ev.probe
	if c := ev.call; c != nil {
		// From here the thread answers for the call if it dies (failCalls),
		// and its PHP finds the request's fiber.
		t.calls[c.id] = c
		if c.fiber != nil {
			t.fibers[c.id] = c.fiber
		}
		t.served.Add(1)
	}
	*n = C.int(len(ev.data))
	return C.CBytes(ev.data)
}

// sleep waits for an event, wait at most (below 0: as long as it takes),
// and reports whether one came. The mark is off: queued() said no.
func (t *thread) sleep(ev *event, wait time.Duration) bool {
	t.box.since.Store(0) // asleep in the eyes of dispatch
	if wait < 0 {
		*ev = <-t.inbox
		return true
	}
	if t.alarm == nil {
		t.alarm = time.NewTimer(wait)
	} else {
		t.alarm.Reset(wait)
	}
	select {
	case *ev = <-t.inbox:
		t.alarm.Stop()
		return true
	case <-t.alarm.C:
		return false
	}
}

// queued reports whether the inbox holds an event, and takes the mark off
// when it does not: a sender that comes from here on sets it after its send,
// and one that came just before is seen by the second look.
func (t *thread) queued() bool {
	t.box.pending.Store(0)
	if len(t.inbox) == 0 {
		return false
	}
	t.box.pending.Store(1)
	return true
}

// goHasEvent is the thread asking whether the mark it found is true: the
// thread that a send woke from its sleep took the event before the mark came.
//
//export goHasEvent
func goHasEvent(tid C.int) C.int {
	if engine.table[int(tid)].Load().queued() {
		return 1
	}
	return 0
}

// goRan is the loop reporting a stretch of PHP, when requests are probed:
// the thread ran for ns nanoseconds since it last came to its loop.
//
//export goRan
func goRan(tid C.int, ns C.longlong) {
	t := engine.table[int(tid)].Load()
	if t.running != nil {
		t.running.Ran(time.Duration(ns))
		t.running = nil
	}
}
