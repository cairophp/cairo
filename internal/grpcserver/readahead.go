package grpcserver

import (
	"context"
	"sync"

	"google.golang.org/grpc/status"

	"github.com/cairophp/cairo/internal/wire"
)

// ReadAhead reads the messages of a stream before PHP asks for them, so that
// one Go operation hands PHP all that has arrived instead of one message.
// What it holds is bounded: at the bound it stops reading, and the sender
// meets HTTP/2's flow control as it does with a reader that is slow.
type ReadAhead struct {
	mu    sync.Mutex
	msgs  []wire.Raw
	size  int
	end   error         // how the stream ended, once it has
	ready chan struct{} // a message or the end is there to take
	room  chan struct{} // a take emptied what was held
}

// The bound: 64 messages, or 1 MiB and the message that passed it.
const (
	aheadMessages = 64
	aheadBytes    = 1 << 20
)

// NewReadAhead starts reading with recv, which returns a message as PHP's
// array, or an error at the end of the stream (io.EOF for a clean one). ctx
// is the stream's: the goroutine ends when recv fails, which it does once
// ctx has ended.
func NewReadAhead(ctx context.Context, recv func() (wire.Raw, error)) *ReadAhead {
	r := &ReadAhead{ready: make(chan struct{}, 1), room: make(chan struct{}, 1)}
	go r.read(ctx, recv)
	return r
}

func (r *ReadAhead) read(ctx context.Context, recv func() (wire.Raw, error)) {
	for {
		msg, err := recv()
		full := r.add(msg, err)
		if err != nil {
			return
		}
		for full && ctx.Err() == nil {
			select {
			case <-r.room:
				full = r.full()
			case <-ctx.Done():
			}
		}
		if full {
			// The stream has ended with nobody taking: what still
			// comes is dropped, and only how it ended is kept.
			for err == nil {
				_, err = recv()
			}
			r.add(nil, err)
			return
		}
	}
}

// add keeps a message, or the end when there is one, and reports whether
// the bound is reached.
func (r *ReadAhead) add(msg wire.Raw, end error) bool {
	r.mu.Lock()
	if end != nil {
		r.end = end
	} else {
		r.msgs = append(r.msgs, msg)
		r.size += len(msg)
	}
	full := r.isFull()
	r.mu.Unlock()
	signal(r.ready)
	return full
}

func (r *ReadAhead) full() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.isFull()
}

func (r *ReadAhead) isFull() bool {
	return len(r.msgs) >= aheadMessages || r.size >= aheadBytes
}

// Take returns the messages that have arrived, in order, and waits for the
// first when there is none. Once all are taken it returns how the stream
// ended, to every caller. A ctx that ends first is that caller's status.
func (r *ReadAhead) Take(ctx context.Context) ([]wire.Raw, error) {
	for {
		r.mu.Lock()
		msgs, end := r.msgs, r.end
		r.msgs, r.size = nil, 0
		r.mu.Unlock()
		if len(msgs) > 0 {
			signal(r.room)
			return msgs, nil
		}
		if end != nil {
			signal(r.ready) // for the callers after this one
			return nil, end
		}
		select {
		case <-r.ready:
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		}
	}
}

func signal(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}
