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

// writerOf is exchangeOf for an operation that writes to the client. The
// request's handler in Go returns without PHP when the client has gone, and
// its ResponseWriter is no longer to be used then: until the caller unlocks
// the event, the handler stays.
func writerOf(ctx context.Context) (*event, error) {
	ev, err := exchangeOf(ctx)
	if err != nil {
		return nil, err
	}
	ev.mu.Lock()
	if ev.ended {
		ev.mu.Unlock()
		return nil, context.Canceled
	}
	return ev, nil
}

// began is what PHP made of a request's client while its call was open,
// instead of answering the call with a response: a streamed answer, a
// WebSocket, events that Go serves. The handler in Go ends it when the call
// returns, and failed says that PHP did not end well.
type began interface {
	finish(failed bool)
}

// end is the handler saying that it is about to return: nothing begins to
// write after it. It returns what PHP began, if anything.
func (ev *event) end() began {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	ev.ended = true
	return ev.began
}

type stream struct {
	id       int64
	w        http.ResponseWriter
	rc       *http.ResponseController
	sends    *duplex.SendQueue
	stall    time.Duration // how long one write may take; 0 for no limit
	stopping atomic.Bool   // the server is stopping: PHP's next piece is refused
}

var (
	streams      sync.Map // int64 -> *stream
	nextStreamID atomic.Int64
)

// ErrStreamClosed is returned for a stream whose request has ended.
var ErrStreamClosed = duplex.ErrClosed

// errStopping refuses a piece as a client that has gone does.
var errStopping = errors.New("the server is stopping")

// stopStreams runs when the server begins to stop. A stream has no end of
// its own for the server to wait for, so each is refused its next piece:
// PHP's handler ends there as it does when its client leaves, what it had
// queued is still written, and the client connects again elsewhere.
func stopStreams() {
	streams.Range(func(_, v any) bool {
		v.(*stream).stopping.Store(true)
		return true
	})
}

// OpenStream begins the answer of the request that ctx is an operation of:
// it sends the status and the headers, and returns the id under which PHP
// sends the body. A stream lasts as long as its handler, so the server's
// write timeout bounds each of its writes instead of the whole answer: a
// client that stops reading is dropped, and one that reads may stay.
func OpenStream(ctx context.Context, status int, header map[string][]string) (int64, error) {
	if status < 200 || status > 599 {
		return 0, fmt.Errorf("a streamed answer cannot have status %d", status)
	}
	ev, err := writerOf(ctx)
	if err != nil {
		return 0, err
	}
	defer ev.mu.Unlock()
	if ev.began != nil {
		return 0, errors.New("the answer has already begun")
	}
	st := ev.openStream(status, header)
	ev.began = st
	streams.Store(st.id, st)
	return st.id, nil
}

// openStream sends the head of an answer whose body follows in pieces.
func (ev *event) openStream(status int, header map[string][]string) *stream {
	st := &stream{id: nextStreamID.Add(1), w: ev.w, rc: http.NewResponseController(ev.w), stall: ev.stall}
	st.sends = duplex.NewSendQueue(st.write, st.flush)
	h := ev.w.Header()
	for k, vs := range header {
		h[http.CanonicalHeaderKey(k)] = vs
	}
	st.extend()
	ev.w.WriteHeader(status)
	st.flush()
	return st
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
	st, err := openStream(id)
	if err != nil {
		return false, err
	}
	return st.sends.TryPut(b)
}

// openStream returns stream id while PHP may still send on it.
func openStream(id int64) (*stream, error) {
	v, ok := streams.Load(id)
	if !ok {
		return nil, ErrStreamClosed
	}
	st := v.(*stream)
	if st.stopping.Load() {
		return nil, errStopping
	}
	return st, nil
}

// Send queues a piece of the body of stream id, waiting for room until ctx
// ends: a client that reads slowly holds its handler back there.
func Send(ctx context.Context, id int64, b []byte) error {
	st, err := openStream(id)
	if err != nil {
		return err
	}
	return st.sends.Put(ctx, b)
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
	if status <= 101 || status > 199 {
		return fmt.Errorf("%d is not the status of a hint", status)
	}
	ev, err := writerOf(ctx)
	if err != nil {
		return err
	}
	defer ev.mu.Unlock()
	if ev.began != nil {
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

// OpenStreamOf and HintOf are OpenStream and Hint for a head that PHP
// encoded as an answer's (decodeAnswer): from C, with no array in between.
func OpenStreamOf(ctx context.Context, head []byte) (int64, error) {
	status, header, err := decodeAnswer(head)
	if err != nil {
		return 0, err
	}
	return OpenStream(ctx, status, header)
}

func HintOf(ctx context.Context, head []byte) error {
	status, header, err := decodeAnswer(head)
	if err != nil {
		return err
	}
	return Hint(ctx, status, header)
}
