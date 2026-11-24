package httpserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cairophp/cairo/internal/duplex"
)

// A streamed answer: PHP sends the head and then the body in pieces, while
// its handler runs and the request's call is still open. The request's
// event is the context that the call runs under, so an operation of PHP
// finds the client it answers there (exchangeOf), and a stream that has
// begun is kept under an id, for the pieces that PHP queues with one call
// and no operation (TrySend).

type exchangeKey struct{}

// Value makes the event the context of its own call: what the request's
// context holds, and the event itself.
func (ev *event) Value(key any) any {
	if _, ok := key.(exchangeKey); ok {
		return ev
	}
	return ev.Context.Value(key)
}

// exchangeOf returns the request that ctx is an operation of, when PHP may
// still reach its client: not for a route whose answers are cached, which
// several requests share.
func exchangeOf(ctx context.Context) (*event, error) {
	ev, _ := ctx.Value(exchangeKey{}).(*event)
	if ev == nil {
		return nil, errors.New("this request's client cannot be reached: its route is cached, or it is not an HTTP request")
	}
	return ev, nil
}

type stream struct {
	id    int64
	w     http.ResponseWriter
	rc    *http.ResponseController
	sends *duplex.SendQueue
	stall time.Duration // how long one write may take; 0 for no limit
}

var (
	streams      sync.Map // int64 -> *stream
	nextStreamID atomic.Int64
)

// ErrStreamClosed is returned for a stream whose request has ended.
var ErrStreamClosed = duplex.ErrClosed

// OpenStream begins the answer of the request that ctx is an operation of:
// it sends the status and the headers, and returns the id under which PHP
// sends the body. A stream lasts as long as its handler, so the server's
// write timeout bounds each of its writes instead of the whole answer: a
// client that stops reading is dropped, and one that reads may stay.
func OpenStream(ctx context.Context, status int, header map[string][]string) (int64, error) {
	ev, err := exchangeOf(ctx)
	if err != nil {
		return 0, err
	}
	if status < 200 || status > 599 {
		return 0, fmt.Errorf("a streamed answer cannot have status %d", status)
	}
	st := &stream{id: nextStreamID.Add(1), w: ev.w, rc: http.NewResponseController(ev.w), stall: ev.stall}
	st.sends = duplex.NewSendQueue(st.write, st.flush)
	if !ev.stream.CompareAndSwap(nil, st) {
		return 0, errors.New("the answer has already begun")
	}
	h := ev.w.Header()
	for k, vs := range header {
		h[http.CanonicalHeaderKey(k)] = vs
	}
	st.extend()
	ev.w.WriteHeader(status)
	st.flush()
	streams.Store(st.id, st)
	return st.id, nil
}

// extend gives the next write its own time.
func (st *stream) extend() {
	if st.stall > 0 {
		_ = st.rc.SetWriteDeadline(time.Now().Add(st.stall)) // a writer without deadlines had none to pass
	}
}

func (st *stream) write(b []byte) error {
	st.extend()
	_, err := st.w.Write(b)
	return err
}

// flush sends what the server buffered: the queue calls it once it has
// written all it held, so pieces that PHP queued together leave together.
func (st *stream) flush() { _ = st.rc.Flush() }

// TrySend queues a piece of the body of stream id if there is room, without
// waiting; queued is false when the queue is full and the caller should use
// Send. b is kept: the caller gives it away.
func TrySend(id int64, b []byte) (queued bool, err error) {
	v, ok := streams.Load(id)
	if !ok {
		return false, ErrStreamClosed
	}
	return v.(*stream).sends.TryPut(b)
}

// Send queues a piece of the body of stream id, waiting for room until ctx
// ends: a client that reads slowly holds its handler back there.
func Send(ctx context.Context, id int64, b []byte) error {
	v, ok := streams.Load(id)
	if !ok {
		return ErrStreamClosed
	}
	return v.(*stream).sends.Put(ctx, b)
}

// finish ends the stream of a request whose call has returned: what PHP
// queued is written first. A handler that failed after the answer began has
// no status left to say so with, so the connection is cut and the client
// sees an answer that is not whole.
func (st *stream) finish(failed bool) {
	streams.Delete(st.id)
	if err := st.sends.Close(); err != nil || failed {
		panic(http.ErrAbortHandler)
	}
}

// Hint sends an informational response (103 Early Hints) with header to
// the client of the request that ctx is an operation of, ahead of its
// answer. The headers are the hint's alone: the answer names its own.
func Hint(ctx context.Context, status int, header map[string][]string) error {
	ev, err := exchangeOf(ctx)
	if err != nil {
		return err
	}
	if status <= 101 || status > 199 {
		return fmt.Errorf("%d is not the status of a hint", status)
	}
	if ev.stream.Load() != nil {
		return errors.New("the answer has already begun")
	}
	h := ev.w.Header()
	for k, vs := range header {
		h[http.CanonicalHeaderKey(k)] = vs
	}
	ev.w.WriteHeader(status)
	for k := range header {
		delete(h, http.CanonicalHeaderKey(k))
	}
	return nil
}
