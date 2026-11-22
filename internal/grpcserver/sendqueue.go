package grpcserver

import (
	"context"
	"sync"
	"sync/atomic"

	"google.golang.org/grpc/status"
)

// SendQueue writes what PHP sends on a stream, in order and on one
// goroutine, so that PHP can queue a message without waiting for it to be
// written. The first write that fails stops the writing, and every put
// after it returns that error.
type SendQueue struct {
	write func([]byte) error

	startOnce sync.Once
	closeMu   sync.RWMutex // held for reading while a message is queued
	closed    bool
	out       chan []byte // encoded messages
	done      chan struct{}
	err       atomic.Pointer[error]
}

// sendQueue is how many messages may wait for the writer; a put beyond it
// waits in the fiber's Go operation (Put) until there is room.
const sendQueue = 64

// NewSendQueue returns a queue that writes each message with write.
func NewSendQueue(write func([]byte) error) *SendQueue {
	return &SendQueue{write: write}
}

func (q *SendQueue) writer() {
	defer close(q.done)
	for m := range q.out {
		if q.err.Load() != nil {
			continue // drain: the other side is gone or the stream broke
		}
		if err := q.write(m); err != nil {
			q.err.Store(&err)
		}
	}
}

// Err is the error of the write that failed, if one has.
func (q *SendQueue) Err() error {
	if err := q.err.Load(); err != nil {
		return *err
	}
	return nil
}

// open returns with closeMu held for reading, so that the queue stays open
// until the caller has queued its message.
func (q *SendQueue) open() error {
	if err := q.Err(); err != nil {
		return err
	}
	q.closeMu.RLock()
	if q.closed {
		q.closeMu.RUnlock()
		return ErrStreamClosed
	}
	q.startOnce.Do(func() {
		q.out = make(chan []byte, sendQueue)
		q.done = make(chan struct{})
		go q.writer()
	})
	return nil
}

// TryPut queues msg if there is room, without waiting; queued is false when
// the queue is full and the caller should use Put.
func (q *SendQueue) TryPut(msg []byte) (queued bool, err error) {
	if err := q.open(); err != nil {
		return false, err
	}
	defer q.closeMu.RUnlock()
	select {
	case q.out <- msg:
		return true, nil
	default:
		return false, nil
	}
}

// Put queues msg, waiting for room until ctx ends.
func (q *SendQueue) Put(ctx context.Context, msg []byte) error {
	if err := q.open(); err != nil {
		return err
	}
	defer q.closeMu.RUnlock()
	select {
	case q.out <- msg:
		return nil
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	}
}

// Close ends the queue and waits until what was queued is written. It
// returns the error of the write that failed, if one has.
func (q *SendQueue) Close() error {
	q.closeMu.Lock()
	first := !q.closed
	q.closed = true
	q.closeMu.Unlock()
	if q.out != nil {
		if first {
			close(q.out)
		}
		<-q.done
	}
	return q.Err()
}
