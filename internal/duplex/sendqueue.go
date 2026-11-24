// Package duplex has the two ends of a stream as PHP has them, whatever
// carries the stream: what PHP sends waits in a SendQueue for the one
// goroutine that writes, and what the other side sends is read ahead of PHP
// by a ReadAhead.
package duplex

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

// ErrClosed is returned for a stream whose owner has closed it.
var ErrClosed = errors.New("the stream has ended")

// SendQueue writes what PHP sends on a stream, in order and on one
// goroutine, so that PHP can queue a message without waiting for it to be
// written. The first write that fails stops the writing, and every put
// after it returns that error.
type SendQueue struct {
	write func([]byte) error
	idle  func() // after a write that left nothing queued, when not nil

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

// NewSendQueue returns a queue that writes each message with write. With
// idle, it calls that once it has written all that was queued: where a
// buffered writer is flushed, so that messages queued together cost one
// flush.
func NewSendQueue(write func([]byte) error, idle func()) *SendQueue {
	return &SendQueue{write: write, idle: idle}
}

func (q *SendQueue) writer() {
	defer close(q.done)
	for m := range q.out {
		if q.err.Load() != nil {
			continue // drain: the other side is gone or the stream broke
		}
		if err := q.write(m); err != nil {
			q.err.Store(&err)
			continue
		}
		if q.idle != nil && len(q.out) == 0 {
			q.idle()
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
		return ErrClosed
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

// Put queues msg, waiting for room until ctx ends: its error is then ctx's.
func (q *SendQueue) Put(ctx context.Context, msg []byte) error {
	if err := q.open(); err != nil {
		return err
	}
	defer q.closeMu.RUnlock()
	select {
	case q.out <- msg:
		return nil
	case <-ctx.Done():
		return ctx.Err()
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
