package core

/*
#include "outbox.h"
*/
import "C"

import (
	"errors"
	"os"
	"unsafe"
)

// openOutbox opens what the PHP threads queue their messages for Go in
// (outbox.h), and returns what Go waits on while it is empty.
func openOutbox() (*os.File, error) {
	fd := C.cairo_outbox_open()
	if fd < 0 {
		return nil, errors.New("cairo: could not open the queue from the PHP threads to Go")
	}
	return os.NewFile(uintptr(fd), "cairo outbox"), nil
}

// collect takes what the PHP threads have for Go, the answers to their
// calls and their own end, for as long as wake is open. A thread hands
// these over without running Go code: it keeps none of Go's processors, and
// Go's scheduler wakes nothing on the thread's account.
func (e *Engine) collect(wake *os.File) {
	var count [8]byte
	for {
		list := C.cairo_outbox_take()
		if list == nil {
			if C.cairo_outbox_wait() == 0 {
				continue
			}
			// Waits as a goroutine, in Go's poller: a thread that queues a
			// message meanwhile writes the descriptor.
			if _, err := wake.Read(count[:]); err != nil {
				return
			}
			continue
		}
		for m := list; m != nil; m = m.next {
			t := e.table[int(m.tid)].Load()
			switch m.kind {
			case C.CAIRO_MSG_ANSWER:
				t.answer(m)
			case C.CAIRO_MSG_EXIT:
				e.threadExited(t, int(m.id))
				e.threadsWG.Done()
			}
		}
		C.cairo_msg_free(list)
	}
}

// answer hands PHP's answer to a call that thread t took to the goroutine
// that waits for it.
func (t *thread) answer(m *C.cairo_msg) {
	id := int64(m.id)
	t.mu.Lock()
	c, ok := t.calls[id]
	// Out of the tables before the answer: Dispatch then reuses both.
	delete(t.calls, id)
	delete(t.fibers, id)
	t.mu.Unlock()
	if !ok {
		return
	}
	t.inflight.Add(-1)
	data := unsafe.Add(unsafe.Pointer(m), C.sizeof_cairo_msg)
	c.reply.meta = C.GoBytes(data, C.int(m.len))
	if m.blen > 0 {
		c.reply.body = C.GoBytes(unsafe.Add(data, m.len), C.int(m.blen))
	}
	c.resp <- &c.reply
}
