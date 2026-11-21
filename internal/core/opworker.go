package core

// The goroutines that run operations.
//
// A goroutine that starts has a small stack, which an operation outgrows a
// few times over (copying it was 15% of the CPU with 64 fibers in Go::call),
// so one that finished an operation waits for the next one of its thread
// instead of ending. A thread keeps maxIdleWorkers of them at most, and
// takes the one that waited the shortest: what a burst leaves beyond that
// ends, and the rest end with the thread.

const maxIdleWorkers = 256

// opWorker is a goroutine that waits for an operation of its thread.
type opWorker chan operation

// worker takes an idle goroutine of the thread, nil when it has none. The
// caller holds t.mu.
func (t *thread) worker() opWorker {
	n := len(t.idle)
	if n == 0 {
		return nil
	}
	w := t.idle[n-1]
	t.idle = t.idle[:n-1]
	return w
}

// start runs o on w, or on a goroutine of its own when w is nil.
func (e *Engine) start(w opWorker, o operation) {
	if w == nil {
		w = make(opWorker, 1)
		go e.work(w, o)
		return
	}
	w <- o // has room: w is on no list but the caller's
}

func (e *Engine) work(w opWorker, o operation) {
	for ok := true; ok; o, ok = <-w {
		e.runOp(o)
		t := o.thread
		t.mu.Lock()
		keep := !t.ended && len(t.idle) < maxIdleWorkers
		if keep {
			t.idle = append(t.idle, w)
		}
		t.mu.Unlock()
		if !keep {
			return
		}
	}
}

// endWorkers ends the goroutines that wait for an operation of thread t,
// which has ended; those in an operation end after it.
func (t *thread) endWorkers() {
	t.mu.Lock()
	for _, w := range t.idle {
		close(w)
	}
	t.idle, t.ended = nil, true
	t.mu.Unlock()
}
