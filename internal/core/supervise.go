package core

/*
#include "loop.h"
#include "outbox.h"
#include "startgate.h"
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
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
//
// A replacement that ends before its loop takes no slot: the code on disk
// does not start (a parse error, an exception, another declaration), and the
// thread it was to replace is the one that still can serve. That one stays,
// and is tried again later.

// Where a thread is with its pool slot (thread.place).
const (
	placed     = iota // in its slot, or an offload thread: restarted when it dies
	placeSpare        // started for a slot that another thread still has
	placeNone         // it ended before its loop as a spare: it is not restarted
)

// maxThreads matches CAIRO_MAX_THREADS in extension.h.
const maxThreads = 4096

// ErrRestartNeeded is what Errors reports, with the reason behind it, when
// no PHP thread can start any more: opcache has a restart of its cache to
// do, which is safe only in a process whose threads have all ended
// (startgate.h). The threads that run serve on. Whoever runs the engine
// stops it and starts the process anew.
var ErrRestartNeeded = errors.New("no PHP thread can start until the process has started anew")

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
	if t.refused.Load() {
		// It ran no PHP, and none that is started after it would: what is
		// queued for a thread whose place it was to take waits for the new
		// process, or for the end of this one.
		e.report(fmt.Errorf("%w: %s", ErrRestartNeeded, noStarts()))
		return
	}
	if !t.came.Load() && (t.place.CompareAndSwap(placeSpare, placeNone) || t.place.Load() == placeNone) {
		// The code on disk does not start, and the thread had no pool slot
		// yet: nothing was queued for it, and the one it was to replace
		// serves on (replace, Reload).
		e.releaseTid(t)
		return
	}
	if t.pool == poolOffload {
		e.jobLost(t)
	}
	// No call is between the thread's tables and its inbox while this is
	// held, and with retiring, nothing is queued here from now on (a crash
	// can come before retire() got as far as retired). keep reads gone
	// under it, to learn whether the thread it gives back still runs.
	t.sendMu.Lock()
	retiring := t.retiring.Load()
	t.retired, t.gone = t.retired || retiring, retiring
	failed := e.failCalls(t, retiring)
	t.sendMu.Unlock()
	if e.isolated && failed > 0 && t.pool == poolWorker {
		// Its request will never be answered: the slot takes the next. Not
		// here, where the thread's answers are collected.
		go releaseSlot(t.slot)
	}
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
	t.info = "" // the thread asks anew: a reload may have come since
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

// noStarts is why no PHP thread starts any more (startgate.h), or "".
func noStarts() string { return C.GoString(C.cairo_startgate_reason()) }

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
	nt, err := e.spare(old.slot)
	if err != nil {
		old.retiring.Store(false)
		e.log.Error("php thread not replaced", "thread", old.id, "reason", reason, "err", err)
		return
	}
	if nt == nil {
		return
	}
	// With an old one that dies meanwhile, the new one gets the slot
	// before it has booted, and requests wait in its inbox.
	select {
	case <-nt.ready:
	case <-old.left:
	case <-e.shutdownCh:
	}
	if nt.refused.Load() {
		return // old serves on, until the process starts anew (ErrRestartNeeded)
	}
	if isClosed(nt.ready) && !nt.came.Load() && !isClosed(old.left) && !e.shuttingDown() {
		nt.place.Store(placeNone)
		e.keep(old, nt, reason)
		return
	}
	if !nt.place.CompareAndSwap(placeSpare, placed) {
		// It ended before its loop and is not restarted (threadExited),
		// and old has left: the slot needs a thread, whatever becomes of it.
		e.replace(old, reason)
		return
	}
	e.swap(old, nt, reason)
}

// keep gives old, whose replacement nt did not start, its place back, and
// asks for a replacement again later: 1s, 2s, 4s and so on, a minute at most.
func (e *Engine) keep(old, nt *thread, reason string) {
	old.sendMu.Lock()
	gone := old.gone // it died while it was retiring: nothing restarts it
	if !gone {
		old.retiring.Store(false)
	}
	old.sendMu.Unlock()
	if gone {
		e.replace(old, reason)
		return
	}
	err := errors.New("the code on disk did not start (see the PHP errors above); the thread serves on with the code it has")
	if nt.differs.Load() {
		err = ErrDeclarationChanged
	}
	again := min(time.Second<<old.kept, time.Minute)
	old.kept = min(old.kept+1, 6)
	e.log.Error("php thread not replaced", "thread", old.id, "reason", reason, "err", err, "again_in", again)
	time.AfterFunc(again, func() { e.retire(old, reason) })
}

func isClosed(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

// spare starts a request thread for a pool slot that another thread still
// has. nil when the engine is stopping.
func (e *Engine) spare(slot int) (*thread, error) {
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
	nt.place.Store(placeSpare)
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
