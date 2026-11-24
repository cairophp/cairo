package core

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// Reloading the PHP code.
//
// A PHP thread has run server.php and holds what it loaded since, so new
// code needs new threads. Reload replaces each request thread the way a
// thread is recycled (retire in supervise.go): its replacement runs
// server.php and comes to its loop, takes the pool slot, and the old thread
// finishes the requests it holds and exits. No request is refused, and none
// waits behind a thread that boots.
//
// The first new thread is a trial, with no slot yet. When it ends before its
// loop, the new code does not start (a parse error, an exception, another
// declaration than the servers were built from): nothing is replaced, and
// the server runs on with the code it has. Only then come the others, a
// quarter of the threads at a time, so that a reload never doubles the
// memory of PHP. The offload threads end after the jobs they hold and start
// anew under their ids, where the jobs that came meanwhile wait.
//
// The new code goes into opcache beside the old, which threads still run.
// When opcache asks for its restart on the way (the cache is full, and what
// the reload replaced is stale), no thread may start any more
// (startgate.h): the reload ends with ErrRestartNeeded, and the new process
// that follows has the new code.

// ErrDeclarationChanged is Reload's error when server.php now declares
// other servers, routes or settings: those are built once, when the process
// starts, so the new code needs a new process.
var ErrDeclarationChanged = errors.New("cairo: server.php declares other servers, routes or settings than the running server has: restart it")

// ErrReloadFailed is Reload's error when the new code does not start. PHP
// has logged why.
var ErrReloadFailed = errors.New("cairo: the new code did not start (see the PHP errors above); the server keeps running the code it has")

const reasonReload = "reload"

// Reload replaces every PHP thread with one that has read the code anew,
// and returns once the new request threads serve. With an error, nothing was
// replaced.
func (e *Engine) Reload() error {
	e.reloadMu.Lock()
	defer e.reloadMu.Unlock()
	e.reloads.Add(1)
	first, err := e.spare(0, true)
	if err != nil || first == nil {
		return err
	}
	select {
	case <-first.ready:
	case <-e.shutdownCh:
		return nil
	}
	if why := noStarts(); why != "" {
		// opcache is about to empty its cache: the new code is served by
		// a new process.
		err := fmt.Errorf("%w: %s", ErrRestartNeeded, why)
		e.report(err)
		return err
	}
	switch {
	case first.came.Load():
	case first.differs.Load():
		return ErrDeclarationChanged
	default:
		return ErrReloadFailed
	}
	if old := e.claim(0); old != nil {
		e.swap(old, first, reasonReload)
	}

	n := len(e.workers)
	batch := make(chan struct{}, (n+3)/4)
	var wg sync.WaitGroup
	for slot := 1; slot < n; slot++ {
		batch <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-batch; wg.Done() }()
			if old := e.claim(slot); old != nil {
				e.replace(old, reasonReload)
			}
		}()
	}
	wg.Wait()
	for i := range e.offload {
		t := e.offload[i].Load()
		t.renewing.Store(true)
		e.post(t, map[string]string{"type": "shutdown"}) // after the jobs it holds
	}
	return nil
}

// claim marks the thread in a slot of the request pool as retiring, for the
// caller to replace. A thread that is being recycled just now is waited
// for: its replacement may have read the old code. nil when the engine is
// stopping.
func (e *Engine) claim(slot int) *thread {
	for {
		old := e.workers[slot].Load()
		if !old.retiring.Swap(true) {
			return old
		}
		select {
		case <-old.swapped:
		case <-time.After(50 * time.Millisecond): // a replacement that could not start gives the thread back
		case <-e.shutdownCh:
			return nil
		}
	}
}
