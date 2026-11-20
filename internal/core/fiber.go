package core

import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"time"
	"unsafe"

	"github.com/cairophp/cairo/internal/wire"
)

// fiber is the goroutine side of one PHP fiber.
type fiber struct {
	id     int64
	ctx    context.Context
	cancel context.CancelFunc
	thread *thread
	ops    chan op
	closed chan struct{} // child fibers only: closed by cairo_fiber_close
	probe  Probe         // its request's, when requests are probed
}

// op is an operation a fiber asked Go to perform while it is suspended. The
// engine's own operations (sleep, park, wait, waitset, offload) use the
// fields; every other one is looked up by name and gets the payload.
type op struct {
	ID      int64   `json:"-"`
	Op      string  `json:"op"`
	Seconds float64 `json:"seconds"`
	Payload string  `json:"payload"`
	Fd      int     `json:"fd"`
	Write   bool    `json:"write"`
	Timeout float64 `json:"timeout"` // seconds; < 0 means none
	Read    []int   `json:"read"`
	WriteFD []int   `json:"write_fds"`
}

// runOp performs one fiber operation on the fiber's goroutine, then wakes the
// fiber on its PHP thread with the result.
func (e *Engine) runOp(f *fiber, o op) {
	var began time.Time
	if f.probe != nil {
		began = f.probe.WaitBegin()
	}
	ev := map[string]any{"type": "io", "io": o.ID}
	switch o.Op {
	case "sleep":
		timer := time.NewTimer(time.Duration(o.Seconds * float64(time.Second)))
		select {
		case <-timer.C:
		case <-f.ctx.Done():
			timer.Stop()
			ev["error"] = CtxError(f.ctx)
		}
	case "park":
		// A fiber blocked on Cairo\Channel or select(): wait until PHP wakes
		// it (cairo_wake), the timeout passes or the context ends.
		ev["result"] = e.park(f.ctx, o.ID, o.Timeout)
		if ev["result"] == "done" {
			delete(ev, "result")
			ev["error"] = CtxError(f.ctx)
		}
	case "offload":
		res, err := e.runOffload(f.ctx, o.Payload)
		if err != nil {
			ev["error"] = CtxError(f.ctx)
		} else {
			ev["result"] = res
		}
	case "wait":
		// A hooked socket read or write: wait until the descriptor is ready.
		ev["result"] = "ready"
		p, err := getPoller()
		if err == nil {
			timeout := time.Duration(-1)
			if o.Timeout >= 0 {
				timeout = time.Duration(o.Timeout * float64(time.Second))
			}
			err = p.wait(f.ctx, o.Fd, o.Write, timeout)
		}
		switch {
		case err == nil:
		case errors.Is(err, errPollTimeout):
			ev["result"] = "timeout"
		case f.ctx.Err() != nil:
			ev["error"] = CtxError(f.ctx)
		default:
			ev["error"] = err.Error()
		}
	case "waitset":
		// A hooked curl transfer: wait for any of curl's sockets.
		ev["result"] = "ready"
		p, err := getPoller()
		if err == nil {
			timeout := time.Duration(-1)
			if o.Timeout >= 0 {
				timeout = time.Duration(o.Timeout * float64(time.Second))
			}
			err = p.waitSet(f.ctx, o.Read, o.WriteFD, timeout)
		}
		switch {
		case err == nil:
		case errors.Is(err, errPollTimeout):
			ev["result"] = "timeout"
		case f.ctx.Err() != nil:
			ev["error"] = CtxError(f.ctx)
		default:
			ev["error"] = err.Error()
		}
	default:
		if h, ok := fiberOps[o.Op]; ok {
			if res, errText := h(f.ctx, []byte(o.Payload)); errText != "" {
				ev["error"] = errText
			} else {
				ev["result"] = res
			}
			break
		}
		h, ok := goOps[o.Op]
		if !ok {
			ev["error"] = "unknown operation " + o.Op
			break
		}
		res, err := h(f.ctx, []byte(o.Payload))
		switch {
		case f.ctx.Err() != nil:
			ev["error"] = CtxError(f.ctx)
		case err != nil:
			ev["error"] = err.Error()
		default:
			ev["result"] = res
		}
	}
	if f.probe != nil {
		f.probe.WaitEnd(o.Op, began)
	}
	f.thread.inbox <- event{data: wire.Encode(ev), probe: f.probe}
}

// CtxError is how an operation tells PHP that its context ended, which PHP
// turns into the context's exception: "deadline_exceeded" or "canceled".
func CtxError(ctx context.Context) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "deadline_exceeded"
	}
	return "canceled"
}

// goFiberOpen gives a new PHP fiber (from Cairo\async) its own goroutine,
// with a child of the parent fiber's context.
//
//export goFiberOpen
func goFiberOpen(tid C.int, parent C.longlong) C.longlong {
	return engine.openFiber(tid, int64(parent), -1)
}

// goContextOpen is goFiberOpen for a derived context (Context::withTimeout(),
// withCancel()): code PHP runs under it sends its operations here, so they
// end with this context. timeout < 0 means none.
//
//export goContextOpen
func goContextOpen(tid C.int, parent C.longlong, timeout C.double) C.longlong {
	return engine.openFiber(tid, int64(parent), float64(timeout))
}

// goContextCancel cancels a derived context. Its goroutine stays until
// goFiberClose, so operations still sent to it fail fast.
//
//export goContextCancel
func goContextCancel(tid C.int, id C.longlong) {
	if f := engine.fiber(tid, int64(id)); f != nil {
		f.cancel()
	}
}

// fiber returns fiber id of the PHP thread that is calling, or nil.
func (e *Engine) fiber(tid C.int, id int64) *fiber {
	t := e.own(tid)
	if t == nil {
		return nil
	}
	return t.fibers[id]
}

func (e *Engine) openFiber(tid C.int, parent int64, timeout float64) C.longlong {
	t := e.own(tid)
	if t == nil {
		return -1
	}
	pctx := context.Background()
	var p Probe
	if pf := t.fibers[parent]; pf != nil {
		pctx, p = pf.ctx, pf.probe
	}
	var ctx context.Context
	var cancel context.CancelFunc
	if timeout >= 0 {
		ctx, cancel = context.WithTimeout(pctx, time.Duration(timeout*float64(time.Second)))
	} else {
		ctx, cancel = context.WithCancel(pctx)
	}
	f := &fiber{
		id: e.nextID.Add(1), ctx: ctx, cancel: cancel, thread: t,
		ops: make(chan op, 1), closed: make(chan struct{}), probe: p,
	}
	t.fibers[f.id] = f
	// The goroutine lives as long as the fiber, even after its context ends:
	// operations then fail fast with the context's error, so the fiber always wakes.
	go func() {
		for {
			select {
			case o := <-f.ops:
				e.runOp(f, o)
			case <-f.closed:
				return
			}
		}
	}()
	return C.longlong(f.id)
}

// goFiberClose ends a fiber's goroutine and cancels its context.
//
//export goFiberClose
func goFiberClose(tid C.int, id C.longlong) {
	f := engine.fiber(tid, int64(id))
	if f == nil {
		return
	}
	delete(f.thread.fibers, f.id)
	f.cancel()
	if f.closed != nil {
		close(f.closed)
	}
}

// goFiberOp hands an operation to the fiber's goroutine and returns its id.
// The PHP fiber suspends right after; the goroutine wakes it when done.
//
//export goFiberOp
func goFiberOp(tid C.int, id C.longlong, data *C.char, n C.int) C.longlong {
	var o op
	if err := json.Unmarshal(C.GoBytes(unsafe.Pointer(data), n), &o); err != nil {
		return -1
	}
	return engine.submit(tid, int64(id), o)
}

// submit hands a decoded operation to the goroutine of the calling thread's
// fiber id; see goFiberOp.
func (e *Engine) submit(tid C.int, id int64, o op) C.longlong {
	f := e.fiber(tid, id)
	if f == nil {
		return -1
	}
	o.ID = e.nextID.Add(1)
	f.ops <- o
	return C.longlong(o.ID)
}
