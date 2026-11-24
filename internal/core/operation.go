package core

import "C"

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/cairophp/cairo/internal/wire"
)

// op is an operation a fiber asked Go to perform while it is suspended,
// looked up by name and given the payload.
type op struct {
	ID      int64
	Op      string
	Payload string
}

// operation is an op with what it runs under and where its result goes. A
// fiber has no goroutine and no context in Go: an operation has a goroutine
// for as long as it takes (opworker.go), and runs under its request's context. These are
// copies of what the request's call holds, because the call is reused once
// PHP has answered, and an operation of a fiber that the request left
// running can end later.
type operation struct {
	op
	ctx     context.Context
	thread  *thread
	request int64
	probe   Probe // the request's, when requests are probed
	scoped  bool  // ctx was made for this operation and is in the thread's table (submit)
}

// runOp performs one operation, then wakes its fiber on the PHP thread with
// the result.
func (e *Engine) runOp(o operation) {
	var began time.Time
	if o.probe != nil {
		began = o.probe.WaitBegin()
	}
	ev := map[string]any{"type": "io", "io": o.ID}
	if h, ok := fiberOps[o.Op]; ok {
		if res, errText := h(o.ctx, []byte(o.Payload)); errText != "" {
			ev["error"] = errText
		} else {
			ev["result"] = res
		}
	} else if h, ok := goOps[o.Op]; !ok {
		ev["error"] = "unknown operation " + o.Op
	} else {
		res, err := h(o.ctx, []byte(o.Payload))
		switch {
		case o.ctx.Err() != nil:
			ev["error"] = CtxError(o.ctx)
		case err != nil:
			ev["error"] = err.Error()
		default:
			ev["result"] = res
		}
	}
	if o.probe != nil {
		o.probe.WaitEnd(o.Op, began)
	}
	if o.scoped {
		o.thread.endScope(o.ID)
	}
	if err := o.thread.send(eventOther, o.request, wire.Encode(ev)); err != nil {
		e.log.Error("result not sent to its php thread", "thread", o.thread.id, "op", o.Op, "err", err)
	}
}

// CtxError is how an operation tells PHP that its context ended, which PHP
// turns into the context's exception: "deadline_exceeded" or "canceled".
func CtxError(ctx context.Context) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "deadline_exceeded"
	}
	return "canceled"
}

// submit starts an operation of a fiber of the calling PHP thread and
// returns the operation's id; the fiber suspends right after, and the
// goroutine that runs the operation wakes it with the result. request is the request the
// fiber belongs to, whose context the operation runs under. scope is what
// PHP derived from that context for the code that asks (Context::goScope()):
// 0 for nothing, a deadline as a Unix time, or +Inf for no deadline of its
// own; an operation with a scope gets a context that PHP can end (goOpCancel).
// Returns -1 for a request that this thread does not hold: the caller is in
// none. A request that PHP has answered comes with its sign turned
// (-request): its fiber runs on (Context::defer(), work after
// fastcgi_finish_request()), and its operations have no context of Go's
// left, which ended with the answer, only the scope PHP gave them.
func (e *Engine) submit(tid C.int, request int64, scope float64, o op) C.longlong {
	t := e.own(tid)
	if t == nil {
		return -1
	}
	run := operation{op: o, ctx: context.Background(), thread: t, request: -request}
	t.mu.Lock()
	if request >= 0 {
		c := t.calls[request]
		if c == nil {
			t.mu.Unlock()
			return -1
		}
		run.ctx, run.request, run.probe = c.ctx, request, c.probe
	}
	w := t.worker()
	t.mu.Unlock()
	run.ID = e.nextID.Add(1)
	if scope != 0 {
		var cancel context.CancelFunc
		if math.IsInf(scope, 1) {
			run.ctx, cancel = context.WithCancel(run.ctx)
		} else {
			sec, frac := math.Modf(scope)
			run.ctx, cancel = context.WithDeadline(run.ctx, time.Unix(int64(sec), int64(frac*1e9)))
		}
		run.scoped = true
		t.mu.Lock()
		t.scoped[run.ID] = cancel
		t.mu.Unlock()
	}
	t.box.handed = 1
	e.start(w, run)
	return C.longlong(run.ID)
}

// endScope frees the context that submit made for operation id.
func (t *thread) endScope(id int64) {
	t.mu.Lock()
	cancel := t.scoped[id]
	delete(t.scoped, id)
	t.mu.Unlock()
	cancel()
}

// cancelOp ends the context that submit made for operation id. An
// operation that is over is not found.
func (t *thread) cancelOp(id int64) {
	t.mu.Lock()
	cancel := t.scoped[id]
	t.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// goOpCancel is PHP canceling a context that operation id runs under
// (Context::cancel()); the calling thread submitted it with a scope.
//
//export goOpCancel
func goOpCancel(tid C.int, id C.longlong) {
	if t := engine.own(tid); t != nil {
		t.cancelOp(int64(id))
	}
}
