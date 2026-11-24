package httpserver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

// The body of a request that PHP reads in pieces while the client sends it
// (a route's ->streamBody()): an upload that is larger than memory, or a
// client that goes on speaking while it is answered. Nothing is read ahead
// of PHP but what the connection buffers, so a handler that reads slowly
// holds its client back.

// maxPiece caps one read: a piece is one allocation.
const maxPiece = 1 << 20

type bodyStream struct {
	mu   sync.Mutex // one read at a time, whichever fibers ask
	body io.Reader
	rc   *http.ResponseController
	err  error // why the body can no longer be read; io.EOF at its end
}

// openBody leaves the body of req to PHP. The server's read timeout is for
// a request that is read before its handler runs, and would end this one in
// the middle; an HTTP/1 connection is read while it is written.
func openBody(w http.ResponseWriter, req *http.Request) *bodyStream {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{}) // a connection without deadlines has none
	_ = rc.EnableFullDuplex()           // HTTP/2 is full duplex as it is
	return &bodyStream{body: req.Body, rc: rc}
}

// ReadBody returns the next piece, of at most size bytes, of the body of the
// request that ctx is an operation of, and io.EOF after the last one. A read
// that ctx ends leaves the body unreadable.
func ReadBody(ctx context.Context, size int) ([]byte, error) {
	ev, err := exchangeOf(ctx)
	if err != nil {
		return nil, err
	}
	if ev.upload == nil {
		return nil, errors.New("the body of this request was read whole: its route has no streamBody()")
	}
	return ev.upload.read(ctx, min(max(size, 1), maxPiece))
}

func (b *bodyStream) read(ctx context.Context, max int) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return nil, b.err
	}
	// A read cannot be told to stop: a deadline that has passed ends it.
	stop := context.AfterFunc(ctx, func() { _ = b.rc.SetReadDeadline(time.Now()) })
	defer stop()
	buf := make([]byte, max)
	n, err := b.body.Read(buf)
	for n == 0 && err == nil {
		n, err = b.body.Read(buf)
	}
	b.err = err
	if n > 0 {
		return buf[:n], nil
	}
	return nil, err
}
