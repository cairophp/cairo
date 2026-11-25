package core

/*
#include "outbox.h"
#include "startgate.h"
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
// calls, that they have booted and their own end, for as long as wake is open. A thread hands
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
		// A message whose body is lent to its call leaves the list: the
		// call frees it, perhaps before this loop reads the next one.
		for m, link := list, &list; m != nil; {
			next := m.next
			t := e.table[int(m.tid)].Load()
			lent := false
			switch m.kind {
			case C.CAIRO_MSG_ANSWER:
				lent = t.answer(m)
			case C.CAIRO_MSG_READY:
				t.came.Store(true)
				t.booted()
			case C.CAIRO_MSG_EXIT:
				t.refused.Store(m.id == C.CAIRO_START_REFUSED)
				t.booted() // retire does not wait for a thread that ended before its loop
				e.threadExited(t, int(m.id))
				e.threadsWG.Done()
			}
			if lent {
				*link = next
			} else {
				link = &m.next
			}
			m = next
		}
		C.cairo_msg_free(list)
	}
}

// answer hands PHP's answer to a call that thread t took to the goroutine
// that waits for it. It reports whether the call now owns m, which a large
// body is lent from instead of being copied here, by the one goroutine that
// collects for all threads.
func (t *thread) answer(m *C.cairo_msg) (lent bool) {
	id := int64(m.id)
	t.mu.Lock()
	c, ok := t.calls[id]
	// Out of the table before the answer: Dispatch then reuses the call.
	delete(t.calls, id)
	t.mu.Unlock()
	if !ok {
		return false
	}
	t.inflight.Add(-1)
	data := unsafe.Add(unsafe.Pointer(m), C.sizeof_cairo_msg)
	c.reply.meta = C.GoBytes(data, C.int(m.len))
	switch body := unsafe.Add(data, m.len); {
	case m.blen >= lendMin:
		c.reply.body = unsafe.Slice((*byte)(body), int(m.blen))
		c.reply.lent = unsafe.Pointer(m)
		lent = true
	case m.blen > 0:
		c.reply.body = C.GoBytes(body, C.int(m.blen))
	}
	// Read before the send: once the reply is taken, c belongs to the next
	// request. Released after it, so that the goroutine of the request that
	// waits for this thread is the one that runs next.
	release := c.release
	c.resp <- &c.reply
	if release != nil {
		release()
	}
	return lent
}
