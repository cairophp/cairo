package core

import "C"

import (
	"errors"
	"time"
)

// Thread supervision.
//
// A PHP thread can die: a fatal error (memory_limit, a stack overflow caught
// by PHP, exit() outside a handler) ends the PHP request the thread runs in,
// and with it every fiber on the thread. The supervisor then answers the
// requests the thread had taken with ErrThreadStopped, closes its fibers'
// goroutines and starts a fresh thread under the same id and inbox, so
// requests still queued for it are served by the new one.
//
// A request thread can also be recycled on purpose, after App::maxRequests()
// requests or once its memory passes App::maxMemory(). PHP asks with
// cairo_retire(); Go starts a replacement, puts it in the old thread's pool
// slot, stops queueing requests to the old thread and sends it a "retire"
// event. The old thread finishes the requests it holds, then exits.

// maxThreads matches CAIRO_MAX_THREADS in extension.h.
const maxThreads = 4096

// ErrThreadStopped is returned for a request whose PHP thread died before it
// answered.
var ErrThreadStopped = errors.New("cairo: the PHP thread serving this call stopped")

// A thread that dies this many times in a row without taking any work in
// between stops the server: it would only keep failing.
const maxCrashLoop = 5

func (e *Engine) allocTid() (int, bool) {
	e.tidMu.Lock()
	defer e.tidMu.Unlock()
	if n := len(e.freeTids); n > 0 {
		tid := e.freeTids[n-1]
		e.freeTids = e.freeTids[:n-1]
		return tid, true
	}
	if e.nextTid >= maxThreads {
		return 0, false
	}
	e.nextTid++
	return e.nextTid - 1, true
}

func (e *Engine) releaseTid(t *thread) {
	e.table[t.id].CompareAndSwap(t, nil)
	e.tidMu.Lock()
	e.freeTids = append(e.freeTids, t.id)
	e.tidMu.Unlock()
}

func (e *Engine) shuttingDown() bool {
	select {
	case <-e.shutdownCh:
		return true
	default:
		return false
	}
}

// threadExited runs on the PHP thread right after it left PHP.
func (e *Engine) threadExited(t *thread, status int) {
	if e.shuttingDown() {
		return
	}
	retiring := t.retiring.Load()
	if retiring {
		// No request may be queued here from now on (a crash can come
		// before retire() got this far).
		t.sendMu.Lock()
		t.retired = true
		t.sendMu.Unlock()
	}
	failed := e.failCalls(t, retiring)
	e.closeFibers(t)

	if retiring && status == 0 {
		e.releaseTid(t)
		e.log.Info("php thread recycled", "thread", t.id, "served", t.served.Load())
		return
	}
	e.log.Error("php thread stopped unexpectedly", "thread", t.id, "exit_status", status, "failed_calls", failed)
	if retiring {
		e.releaseTid(t) // its replacement is already running
		return
	}

	if t.served.Load() == t.bootMark {
		t.crashes++
	} else {
		t.crashes = 0
	}
	if t.crashes >= maxCrashLoop {
		e.report(errors.New("php thread keeps stopping right after it starts; see the PHP errors above"))
		return
	}
	e.lifeMu.Lock()
	if e.shuttingDown() {
		e.lifeMu.Unlock()
		return
	}
	e.threadsWG.Add(1)
	e.lifeMu.Unlock()

	delay := time.Duration(0)
	if t.crashes > 0 {
		delay = min(100*time.Millisecond<<t.crashes, 5*time.Second)
	}
	t.bootMark = t.served.Load()
	go func() {
		time.Sleep(delay)
		if err := e.startThread(t); err != nil {
			e.threadsWG.Done()
			e.report(err)
			return
		}
		e.log.Warn("php thread restarted", "thread", t.id)
	}()
}

// failCalls answers, with nil, the calls thread t took and, with queued,
// those still in its inbox, which no thread will take. It returns how many.
func (e *Engine) failCalls(t *thread, queued bool) int {
	n := len(t.calls)
	for id, c := range t.calls {
		// Out of the tables before the answer: Dispatch then reuses both.
		delete(t.calls, id)
		delete(t.fibers, id)
		c.resp <- nil
	}
	for queued {
		select {
		case ev := <-t.inbox:
			if ev.call != nil {
				ev.call.resp <- nil
				n++
			}
		default:
			queued = false
		}
	}
	t.inflight.Add(int64(-n))
	return n
}

// closeFibers ends the goroutines of the async() fibers that lived on t.
// Request fibers end with their call.
func (e *Engine) closeFibers(t *thread) {
	for _, f := range t.fibers {
		if f.closed != nil {
			f.cancel()
			close(f.closed)
		}
	}
	clear(t.fibers)
}

// retire replaces request thread old with a fresh one, then lets old finish
// what it holds and exit.
func (e *Engine) retire(old *thread, reason string) {
	if old.pool != poolWorker || old.retiring.Swap(true) {
		return
	}
	e.lifeMu.Lock()
	if e.shuttingDown() {
		e.lifeMu.Unlock()
		return
	}
	tid, ok := e.allocTid()
	if !ok {
		e.lifeMu.Unlock()
		old.retiring.Store(false)
		e.log.Warn("php thread not recycled: no free thread ids", "thread", old.id)
		return
	}
	nt := e.newThread(tid, poolWorker, old.slot)
	e.threadsWG.Add(1)
	e.lifeMu.Unlock()
	if err := e.startThread(nt); err != nil {
		e.threadsWG.Done()
		e.releaseTid(nt)
		old.retiring.Store(false)
		e.log.Error("php thread not recycled", "thread", old.id, "err", err)
		return
	}
	e.workers[old.slot].Store(nt)
	old.sendMu.Lock() // waits for requests being queued to old
	old.retired = true
	old.sendMu.Unlock()
	e.post(old, map[string]string{"type": "retire"}) // after every request queued to old
	e.log.Info("recycling php thread", "thread", old.id, "reason", reason, "served", old.served.Load(), "replacement", tid)
}

func (e *Engine) report(err error) {
	select {
	case e.threadErrs <- err:
	default:
	}
}

//export goThreadDone
func goThreadDone(tid C.int, status C.int) {
	e := engine
	defer e.threadsWG.Done()
	e.threadExited(e.table[int(tid)].Load(), int(status))
}

//export goThreadRetire
func goThreadRetire(tid C.int, reason *C.char, n C.int) {
	e := engine
	t := e.table[int(tid)].Load()
	why := C.GoStringN(reason, n)
	// Not on this thread: putting the replacement in place can wait for
	// senders that wait on this thread's inbox.
	go e.retire(t, why)
}
