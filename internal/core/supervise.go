package core

/*
#include "loop.h"
#include "outbox.h"
*/
import "C"

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Thread supervision.
//
// A PHP thread can die: a fatal error (memory_limit, a stack overflow caught
// by PHP, exit() outside a handler) ends the PHP request the thread runs in,
// and with it every fiber on the thread. The supervisor then answers the
// requests the thread had taken with ErrThreadStopped, which ends their
// contexts and with them what their fibers had asked Go for, and starts a
// fresh thread under the same id and with it the same inbox, so requests
// still queued for it are served by the new one.
//
// A request thread can also be recycled on purpose, after App::maxRequests()
// requests or once its memory passes App::maxMemory(). PHP asks with
// cairo_retire(); Go starts a replacement and, once that has run server.php
// and come to its loop, puts it in the old thread's pool slot, stops queueing
// requests to the old thread and sends it a "retire" event. The old thread
// serves until then, so no request waits behind the boot of the new one,
// finishes the requests it holds, then exits. A reload replaces every thread
// that way (reload.go).

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

// threadExited runs once the PHP thread has left PHP and Go has all that
// the thread answered before (collect).
func (e *Engine) threadExited(t *thread, status int) {
	t.endWorkers()
	if e.shuttingDown() {
		return
	}
	if t.trial.Load() && !t.came.Load() {
		// The first thread of a reload, and the new code does not start:
		// it had no pool slot, so nothing was queued for it (Reload).
		e.releaseTid(t)
		return
	}
	if t.pool == poolOffload {
		e.jobLost(t)
	}
	retiring := t.retiring.Load()
	// No call is between the thread's tables and its inbox while this is
	// held, and with retiring, nothing is queued here from now on (a crash
	// can come before retire() got as far as retired).
	t.sendMu.Lock()
	t.retired, t.gone = t.retired || retiring, retiring
	failed := e.failCalls(t, retiring)
	t.sendMu.Unlock()
	if retiring {
		close(t.left)
	}

	if retiring && status == 0 {
		e.releaseTid(t)
		e.counts.recycled.Add(1)
		e.log.Log(context.Background(), t.level(), "php thread recycled", "thread", t.id, "served", t.box.served.Load()-t.bootMark)
		return
	}
	// An offload thread that a reload told to end starts anew under its id.
	renewed := t.renewing.Swap(false) && status == 0
	if !renewed {
		e.log.Error("php thread stopped unexpectedly", "thread", t.id, "exit_status", status, "failed_calls", failed)
	}
	if retiring {
		e.releaseTid(t) // its replacement is already running
		return
	}

	if t.box.served.Load() == t.bootMark && !renewed {
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
	t.bootMark = t.box.served.Load()
	t.mu.Lock()
	t.ended = false
	t.mu.Unlock()
	go func() {
		time.Sleep(delay)
		if err := e.startThread(t); err != nil {
			e.threadsWG.Done()
			e.report(err)
			return
		}
		if renewed {
			return
		}
		e.counts.restarts.Add(1)
		e.log.Warn("php thread restarted", "thread", t.id)
	}()
}

// level is where the log says that the thread was replaced: a reload says
// it once, for all of its threads.
func (t *thread) level() slog.Level {
	if t.quiet.Load() {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

// failCalls answers, with nil, the calls that thread t read and, with
// queued, those whose events are still in its inbox, which no thread will
// take. It returns how many. The thread has ended, and the caller holds
// sendMu.
func (e *Engine) failCalls(t *thread, queued bool) int {
	var waiting map[int64]bool
	if queued {
		C.cairo_msg_free(C.cairo_inbox_drain(C.int(t.id)))
	} else {
		waiting = t.queuedCalls()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for id, c := range t.calls {
		if waiting[id] {
			continue
		}
		// Out of the table before the answer: Dispatch then reuses the call.
		delete(t.calls, id)
		c.resp <- nil
		n++
	}
	t.inflight.Add(int64(-n))
	return n
}

// retire replaces request thread old with a fresh one, then lets old finish
// what it holds and exit.
func (e *Engine) retire(old *thread, reason string) {
	if old.pool != poolWorker || old.retiring.Swap(true) {
		return
	}
	e.replace(old, reason)
}

// replace is retire for a thread that the caller has marked as retiring.
func (e *Engine) replace(old *thread, reason string) {
	nt, err := e.spare(old.slot, false)
	if err != nil {
		old.retiring.Store(false)
		e.log.Error("php thread not recycled", "thread", old.id, "err", err)
		return
	}
	if nt == nil {
		return
	}
	// A new thread that ends before its loop is restarted like any other
	// (threadExited), in the slot: the old one has asked to leave. With an
	// old one that died meanwhile, requests wait in the new one's inbox.
	select {
	case <-nt.ready:
	case <-old.left:
	case <-e.shutdownCh:
	}
	e.swap(old, nt, reason)
}

// spare starts a request thread for a pool slot that another thread still
// has. nil when the engine is stopping.
func (e *Engine) spare(slot int, trial bool) (*thread, error) {
	e.lifeMu.Lock()
	if e.shuttingDown() {
		e.lifeMu.Unlock()
		return nil, nil
	}
	tid, ok := e.allocTid()
	if !ok {
		e.lifeMu.Unlock()
		return nil, errors.New("no free thread ids")
	}
	nt := e.newThread(tid, poolWorker, slot)
	nt.trial.Store(trial)
	e.threadsWG.Add(1)
	e.lifeMu.Unlock()
	if err := e.startThread(nt); err != nil {
		e.threadsWG.Done()
		e.releaseTid(nt)
		return nil, err
	}
	return nt, nil
}

// swap puts nt in the pool slot of old, which then gets no more requests,
// finishes those it holds and exits.
func (e *Engine) swap(old, nt *thread, reason string) {
	old.quiet.Store(reason == reasonReload)
	e.workers[old.slot].Store(nt)
	old.sendMu.Lock() // waits for requests being queued to old
	old.retired = true
	old.sendMu.Unlock()
	close(old.swapped)
	e.post(old, map[string]string{"type": "retire"}) // after every request queued to old
	e.log.Log(context.Background(), old.level(), "recycling php thread", "thread", old.id, "reason", reason, "served", old.box.served.Load()-old.bootMark, "replacement", nt.id)
}

// booted marks that the thread came to its loop, or ended before it did.
func (t *thread) booted() {
	if !t.up {
		t.up = true
		close(t.ready)
	}
}

func (e *Engine) report(err error) {
	select {
	case e.threadErrs <- err:
	default:
	}
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
