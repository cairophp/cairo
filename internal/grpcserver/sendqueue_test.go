package grpcserver

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// sink is where a queue under test writes: it keeps what was written, and
// its writes wait while hold is locked.
type sink struct {
	hold    sync.Mutex
	mu      sync.Mutex
	written []string
	fail    error // returned by every write once set
}

func (s *sink) write(msg []byte) error {
	s.hold.Lock()
	defer s.hold.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	s.written = append(s.written, string(msg))
	return nil
}

func (s *sink) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.written)
}

// Messages are written in the order they were queued, by puts that wait
// and puts that do not, and Close returns once all are written.
func TestSendQueueKeepsOrder(t *testing.T) {
	var s sink
	q := NewSendQueue(s.write)
	var want []string
	for i := 0; i < 1000; i++ {
		msg := strconv.Itoa(i)
		want = append(want, msg)
		queued, err := q.TryPut([]byte(msg))
		if err == nil && !queued {
			err = q.Put(context.Background(), []byte(msg))
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	if got := s.all(); !slices.Equal(got, want) {
		t.Fatalf("%d messages were written, the first %.5q", len(got), got)
	}
	if err := q.Close(); err != nil {
		t.Fatalf("a second close: %v", err)
	}
	if _, err := q.TryPut(nil); !errors.Is(err, ErrStreamClosed) {
		t.Fatalf("a put after the close: %v", err)
	}
}

// A writer that does not get on leaves room for sendQueue messages and the
// one it holds; a put beyond them waits until its context ends.
func TestSendQueueIsBounded(t *testing.T) {
	var s sink
	s.hold.Lock()
	q := NewSendQueue(s.write)
	queued := 0
	for i := 0; i < 3*sendQueue; i++ {
		ok, err := q.TryPut([]byte{1})
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			queued++
		}
	}
	if queued < sendQueue || queued > sendQueue+1 {
		t.Fatalf("%d messages were queued, the bound is %d", queued, sendQueue)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// The writer may take one message between the puts above and this one.
	err := q.Put(ctx, []byte{1})
	if err == nil {
		err = q.Put(ctx, []byte{1})
	}
	if status.Code(err) != codes.Canceled {
		t.Fatalf("a put with no room and a context that ended: %v", err)
	}
	s.hold.Unlock()
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

// The write that fails is the last: what is queued behind it is dropped,
// and the puts and the close after it return its error.
func TestSendQueueStopsAtAFailedWrite(t *testing.T) {
	broken := errors.New("the client went away")
	var s sink
	s.hold.Lock()
	q := NewSendQueue(s.write)
	for i := 0; i < 10; i++ {
		if _, err := q.TryPut([]byte{1}); err != nil {
			t.Fatal(err)
		}
	}
	s.mu.Lock()
	s.fail = broken
	s.mu.Unlock()
	s.hold.Unlock()
	if err := q.Close(); !errors.Is(err, broken) {
		t.Fatalf("the close returned %v", err)
	}
	if got := s.all(); len(got) != 0 {
		t.Fatalf("%d messages were written after the write that failed", len(got))
	}

	q = NewSendQueue(s.write)
	if err := q.Put(context.Background(), []byte{1}); err != nil {
		t.Fatal(err)
	}
	until(t, "the write has failed", func() bool { return q.Err() != nil })
	if _, err := q.TryPut([]byte{1}); !errors.Is(err, broken) {
		t.Fatalf("a put after the write that failed: %v", err)
	}
	if err := q.Put(context.Background(), []byte{1}); !errors.Is(err, broken) {
		t.Fatalf("a waiting put after the write that failed: %v", err)
	}
	_ = q.Close()
}
