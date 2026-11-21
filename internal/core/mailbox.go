package core

/*
#include "loop.h"
*/
import "C"

import (
	"errors"
	"runtime"
	"sync/atomic"
	"time"
	"unsafe"
)

// mailbox is what Go shares with the loop of a PHP thread, which runs in C
// (cairo_mailbox in loop.h, the same memory): the thread reads and writes it
// at every turn of its loop without a call into Go. What is C's alone has no
// name here.
type mailbox struct {
	// When the thread last came back to its loop (unix ns; 0 while it
	// sleeps): dispatch passes over a thread that has run PHP for too long
	// (leastBusy), and extensions ask which threads are busy (EachBusyThread).
	since atomic.Int64
	// A moving average of how long the thread runs PHP between two visits
	// to its loop, in nanoseconds: what a call queued behind others waits
	// for each of them.
	stretch atomic.Int64
	// How many events the thread has taken out of its inbox, and how many
	// of them began a call.
	took   atomic.Int64
	served atomic.Int64
	// The events the thread took and has not read yet, oldest first. Go
	// reads it once the thread has ended (queuedCalls).
	next *C.cairo_msg
	// 1 when the thread gave Go an operation or an answer since it last
	// came to its loop: a goroutine has to take that over, and the thread
	// looks before it sleeps (look). Not after a socket wait: the thread
	// watched the socket itself before it asked Go. Only the thread itself
	// reads and writes it, in C and in its calls into Go.
	handed int32
	_      [20]byte // the thread's fields and the senders' share no cache line
	// The events queued for the thread (queue.h), which C pushes for send.
	inbox unsafe.Pointer
	// How many events were ever queued for the thread.
	pushed atomic.Int64
	_      [48]byte
}

// The two layouts are one: neither size, and neither place of the inbox, may
// exceed the other.
var (
	_ [unsafe.Sizeof(mailbox{}) - uintptr(C.sizeof_cairo_mailbox)]struct{}
	_ [uintptr(C.sizeof_cairo_mailbox) - unsafe.Sizeof(mailbox{})]struct{}
	_ [unsafe.Offsetof(mailbox{}.inbox) - uintptr(C.cairo_mailbox_inbox_at)]struct{}
	_ [uintptr(C.cairo_mailbox_inbox_at) - unsafe.Offsetof(mailbox{}.inbox)]struct{}
)

// look is how long a thread watches its inbox before it sleeps, when it
// handed Go something on its way (look() in loop.c). It has to outlast the
// wake-up of the Go thread that takes that over, a few microseconds; what
// it costs is that much CPU for each sleep that it did not spare.
const look = 6 * time.Microsecond

// setLook turns the look on where a second CPU can fill the inbox
// meanwhile: with one, the thread would only keep it from doing so.
func setLook() {
	if runtime.NumCPU() > 1 {
		C.cairo_loop_look(C.int64_t(look))
	}
}

func mailboxOf(tid int) *mailbox {
	return (*mailbox)(unsafe.Pointer(C.cairo_mailbox_of(C.int(tid))))
}

// Whether an event begins a call, as loop.h has it.
const (
	eventOther = int(C.CAIRO_EVENT)
	eventCall  = int(C.CAIRO_EVENT_CALL)
)

var errNoMemory = errors.New("cairo: no memory to queue an event for a PHP thread")

// send queues an event (internal/wire) for the thread, which is copied, and
// wakes the thread when it sleeps. kind says whether the event begins a
// call; id is that call, or the fiber the event is for, or 0. An event for a
// thread that has ended is dropped: its thread id may be another's by now.
func (t *thread) send(kind int, id int64, data []byte) error {
	t.sendMu.RLock()
	defer t.sendMu.RUnlock()
	return t.push(kind, id, data)
}

// push is send for a caller that holds sendMu.
func (t *thread) push(kind int, id int64, data []byte) error {
	if t.gone {
		return nil
	}
	if C.cairo_inbox_push(C.int(t.id), C.int(kind), C.int64_t(id), (*C.char)(unsafe.Pointer(unsafe.SliceData(data))), C.size_t(len(data))) != 0 {
		return errNoMemory
	}
	return nil
}

// queued is how many events wait for the thread.
func (t *thread) queued() int64 {
	return t.box.pushed.Load() - t.box.took.Load()
}

// queuedCalls is the calls whose events the thread has not read. Only for a
// thread that has ended: nothing takes an event while this reads them.
func (t *thread) queuedCalls() map[int64]bool {
	ids := map[int64]bool{}
	for _, m := range []*C.cairo_msg{t.box.next, (*C.cairo_msg)(atomic.LoadPointer(&t.box.inbox))} {
		for ; m != nil; m = m.next {
			if int(m.kind) == eventCall {
				ids[int64(m.id)] = true
			}
		}
	}
	return ids
}

// goTook is the loop taking an event, when requests are probed: id is the
// fiber it is for, whose request the stretch of PHP that begins belongs to.
//
//export goTook
func goTook(tid C.int, id C.longlong) {
	t := engine.table[int(tid)].Load()
	t.running = nil
	if f := engine.fiber(tid, int64(id)); f != nil {
		t.running = f.probe
	}
}

// goRan is the loop reporting a stretch of PHP, when requests are probed:
// the thread ran for ns nanoseconds since it last came to its loop. An
// answer reports the stretch up to it first (cairo_loop_answered), so that
// the request's probe has it when the request is done.
//
//export goRan
func goRan(tid C.int, ns C.longlong) {
	t := engine.table[int(tid)].Load()
	if t.running != nil {
		t.running.Ran(time.Duration(ns))
		t.running = nil
	}
}
