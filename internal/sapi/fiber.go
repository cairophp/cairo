package sapi

/*
#include <stdlib.h>
#include "pgsql.h"
*/
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
	timing *reqTiming    // its request's waits, with App::slowRequests() on
}

// op is an operation a fiber asked Go to perform while it is suspended.
type op struct {
	ID      int64       `json:"-"`
	Op      string      `json:"op"`
	Seconds float64     `json:"seconds"`
	Payload string      `json:"payload"`
	Fd      int         `json:"fd"`
	Write   bool        `json:"write"`
	Timeout float64     `json:"timeout"` // seconds; < 0 means none
	Network string      `json:"network"`
	Address string      `json:"address"`
	Host    string      `json:"host"`
	Read    []int       `json:"read"`
	WriteFD []int       `json:"write_fds"`
	TLS     *tlsOptions `json:"tls"`
}

// runOp performs one fiber operation on the fiber's goroutine, then wakes the
// fiber on its PHP thread with the result.
func (e *Engine) runOp(f *fiber, o op) {
	var began time.Time
	if f.timing != nil {
		began = f.timing.begin()
	}
	ev := map[string]any{"type": "io", "io": o.ID}
	switch o.Op {
	case "sleep":
		timer := time.NewTimer(time.Duration(o.Seconds * float64(time.Second)))
		select {
		case <-timer.C:
		case <-f.ctx.Done():
			timer.Stop()
			ev["error"] = ctxError(f.ctx)
		}
	case "park":
		// A fiber blocked on Cairo\Channel or select(): wait until PHP wakes
		// it (cairo_wake), the timeout passes or the context ends.
		ev["result"] = e.park(f.ctx, o.ID, o.Timeout)
		if ev["result"] == "done" {
			delete(ev, "result")
			ev["error"] = ctxError(f.ctx)
		}
	case "offload":
		res, err := e.runOffload(f.ctx, o.Payload)
		if err != nil {
			ev["error"] = ctxError(f.ctx)
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
			ev["error"] = ctxError(f.ctx)
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
			ev["error"] = ctxError(f.ctx)
		default:
			ev["error"] = err.Error()
		}
	case "dial":
		// A hooked connect: DNS and connect run here, on the fiber's goroutine.
		timeout := time.Duration(0)
		if o.Timeout > 0 {
			timeout = time.Duration(o.Timeout * float64(time.Second))
		}
		var fd int
		var err error
		if o.Network == "tls" && o.TLS != nil {
			fd, err = dialTLS(f.ctx, o.Address, timeout, *o.TLS)
		} else {
			fd, err = dialFD(f.ctx, o.Network, o.Address, timeout)
		}
		switch {
		case err == nil:
			ev["result"] = map[string]any{"fd": fd}
		case f.ctx.Err() != nil:
			ev["error"] = ctxError(f.ctx)
		default:
			ev["result"] = map[string]any{"err": err.Error()}
		}
	case "starttls":
		// stream_socket_enable_crypto(): the handshake runs here.
		timeout := time.Duration(0)
		if o.Timeout > 0 {
			timeout = time.Duration(o.Timeout * float64(time.Second))
		}
		var opts tlsOptions
		if o.TLS != nil {
			opts = *o.TLS
		}
		fd, err := startTLS(f.ctx, o.Fd, o.Host, timeout, opts)
		switch {
		case err == nil:
			ev["result"] = map[string]any{"fd": fd}
		case f.ctx.Err() != nil:
			ev["error"] = ctxError(f.ctx)
		default:
			ev["result"] = map[string]any{"err": err.Error()}
		}
	case "stoptls":
		// stream_socket_enable_crypto($s, false): the TLS shutdown runs here.
		timeout := time.Duration(0)
		if o.Timeout > 0 {
			timeout = time.Duration(o.Timeout * float64(time.Second))
		}
		fd, err := stopTLS(f.ctx, o.Fd, timeout)
		switch {
		case err == nil:
			ev["result"] = map[string]any{"fd": fd}
		case f.ctx.Err() != nil:
			ev["error"] = ctxError(f.ctx)
		default:
			ev["result"] = map[string]any{"err": err.Error()}
		}
	case "pg":
		// A PDO pgsql call: runs here, on the fiber's goroutine, so the
		// request's deadline cancels the query.
		req, err := pgRequestFrom([]byte(o.Payload))
		if err != nil {
			ev["error"] = err.Error()
			break
		}
		resp := pgDo(f.ctx, req)
		if f.ctx.Err() != nil {
			if id, ok := resp["result"].(int64); ok {
				if r := goPgTake(C.longlong(id)); r != nil {
					C.cairo_pg_result_free(r)
				}
			}
			ev["error"] = ctxError(f.ctx)
			break
		}
		ev["result"] = resp
	case "lookup":
		ips, err := lookupIPv4(f.ctx, o.Host)
		switch {
		case err == nil:
			ev["result"] = ips
		case f.ctx.Err() != nil:
			ev["error"] = ctxError(f.ctx)
		default:
			ev["result"] = []string{}
		}
	default:
		h, ok := goOps[o.Op]
		if !ok {
			ev["error"] = "unknown operation " + o.Op
			break
		}
		res, err := h(f.ctx, []byte(o.Payload))
		switch {
		case f.ctx.Err() != nil:
			ev["error"] = ctxError(f.ctx)
		case err != nil:
			ev["error"] = err.Error()
		default:
			ev["result"] = res
		}
	}
	if f.timing != nil {
		f.timing.end(opKind(o.Op), began)
	}
	f.thread.inbox <- event{data: wire.Encode(ev), timing: f.timing}
}

func ctxError(ctx context.Context) string {
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
	return engine.openFiber(int(tid), int64(parent), -1)
}

// goContextOpen is goFiberOpen for a derived context (Context::withTimeout(),
// withCancel()): code PHP runs under it sends its operations here, so they
// end with this context. timeout < 0 means none.
//
//export goContextOpen
func goContextOpen(tid C.int, parent C.longlong, timeout C.double) C.longlong {
	return engine.openFiber(int(tid), int64(parent), float64(timeout))
}

// goContextCancel cancels a derived context. Its goroutine stays until
// goFiberClose, so operations still sent to it fail fast.
//
//export goContextCancel
func goContextCancel(id C.longlong) {
	if v, ok := engine.fibers.Load(int64(id)); ok {
		v.(*fiber).cancel()
	}
}

func (e *Engine) openFiber(tid int, parent int64, timeout float64) C.longlong {
	pctx := context.Background()
	var timing *reqTiming
	if v, ok := e.fibers.Load(parent); ok {
		pctx, timing = v.(*fiber).ctx, v.(*fiber).timing
	}
	var ctx context.Context
	var cancel context.CancelFunc
	if timeout >= 0 {
		ctx, cancel = context.WithTimeout(pctx, time.Duration(timeout*float64(time.Second)))
	} else {
		ctx, cancel = context.WithCancel(pctx)
	}
	f := &fiber{
		id: e.nextID.Add(1), ctx: ctx, cancel: cancel, thread: e.table[int(tid)].Load(),
		ops: make(chan op, 1), closed: make(chan struct{}), timing: timing,
	}
	e.fibers.Store(f.id, f)
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
func goFiberClose(id C.longlong) {
	if v, ok := engine.fibers.LoadAndDelete(int64(id)); ok {
		f := v.(*fiber)
		f.cancel()
		if f.closed != nil {
			close(f.closed)
		}
	}
}

// goFiberOp hands an operation to the fiber's goroutine and returns its id.
// The PHP fiber suspends right after; the goroutine wakes it when done.
//
//export goFiberOp
func goFiberOp(id C.longlong, data *C.char, n C.int) C.longlong {
	e := engine
	opID := e.nextID.Add(1)
	v, ok := e.fibers.Load(int64(id))
	if !ok {
		return -1
	}
	var o op
	if err := json.Unmarshal(C.GoBytes(unsafe.Pointer(data), n), &o); err != nil {
		return -1
	}
	o.ID = opID
	v.(*fiber).ops <- o
	return C.longlong(opID)
}

// submit hands a decoded operation to fiber id's goroutine; see goFiberOp.
func (e *Engine) submit(id int64, o op) C.longlong {
	v, ok := e.fibers.Load(id)
	if !ok {
		return -1
	}
	o.ID = e.nextID.Add(1)
	v.(*fiber).ops <- o
	return C.longlong(o.ID)
}
