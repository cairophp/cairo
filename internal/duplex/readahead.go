package duplex

import (
	"context"
	"sync"
)

// ReadAhead reads the messages of a stream before PHP asks for them, so that
// one Go operation hands PHP all that has arrived instead of one message.
// What it holds is bounded: at the bound it stops reading, and the sender
// meets HTTP/2's flow control as it does with a reader that is slow.
type ReadAhead[M ~[]byte] struct {
	mu    sync.Mutex
	msgs  []M
	size  int
	end   error         // how the stream ended, once it has
	ready chan struct{} // a message or the end is there to take
	room  chan struct{} // a take emptied what was held
	done  chan struct{} // closed once the end is kept: nothing more is read
}

// The bound: 64 messages, or 1 MiB and the message that passed it.
const (
	aheadMessages = 64
	aheadBytes    = 1 << 20
)

// NewReadAhead starts reading with recv, which returns a message as PHP
// gets it, or an error at the end of the stream (io.EOF for a clean one). ctx
// is the stream's: the goroutine ends when recv fails, which it does once
// ctx has ended.
func NewReadAhead[M ~[]byte](ctx context.Context, recv func() (M, error)) *ReadAhead[M] {
	r := &ReadAhead[M]{ready: make(chan struct{}, 1), room: make(chan struct{}, 1), done: make(chan struct{})}
	go r.read(ctx, recv)
	return r
}

// Done is closed when the stream has ended and how it ended is there to
// take, behind the messages that came before.
func (r *ReadAhead[M]) Done() <-chan struct{} { return r.done }

func (r *ReadAhead[M]) read(ctx context.Context, recv func() (M, error)) {
	defer close(r.done)
	for {
		msg, err := recv()
		full := r.add(msg, err)
		if err != nil {
			return
		}
		for full && ctx.Err() == nil {
			select {
			case <-r.room:
				full = r.Full()
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
func (r *ReadAhead[M]) add(msg M, end error) bool {
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

// Full reports whether the bound is reached: nothing is read from the
// stream until PHP has taken what is held.
func (r *ReadAhead[M]) Full() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.isFull()
}

func (r *ReadAhead[M]) isFull() bool {
	return len(r.msgs) >= aheadMessages || r.size >= aheadBytes
}

// Take returns the messages that have arrived, in order, and waits for the
// first when there is none. Once all are taken it returns how the stream
// ended, to every caller. A ctx that ends first is that caller's error.
func (r *ReadAhead[M]) Take(ctx context.Context) ([]M, error) {
	for {
		if msgs, end := r.TryTake(); len(msgs) > 0 || end != nil {
			return msgs, end
		}
		if err := r.Wait(ctx); err != nil {
			return nil, err
		}
	}
}

// TryTake is Take without the wait: it returns nothing when no message has
// arrived and the stream has not ended.
func (r *ReadAhead[M]) TryTake() ([]M, error) {
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
	}
	return nil, end
}

// Wait returns when there may be something to take, or with ctx's error.
func (r *ReadAhead[M]) Wait(ctx context.Context) error {
	select {
	case <-r.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func signal(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}
