package duplex

import (
	"context"
	"errors"
	"io"
	"strconv"
	"testing"
	"time"
)

// source is a stream for the tests: recv returns what was put in feed, then
// io.EOF once feed is closed, and the stream's error once its context ends.
type source struct {
	ctx  context.Context
	feed chan []byte
}

func newSource(t *testing.T, messages int) (*source, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := &source{ctx, make(chan []byte, messages)}
	for i := 0; i < messages; i++ {
		s.feed <- []byte(strconv.Itoa(i))
	}
	return s, cancel
}

func (s *source) recv() ([]byte, error) {
	select {
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	default:
	}
	select {
	case m, ok := <-s.feed:
		if !ok {
			return nil, io.EOF
		}
		return m, nil
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
}

func until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
	}
}

// takeAll takes until the stream's end and returns what came before it.
func takeAll(t *testing.T, r *ReadAhead[[]byte]) ([]string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var got []string
	for {
		msgs, err := r.Take(ctx)
		if err != nil {
			return got, err
		}
		if len(msgs) == 0 {
			t.Fatal("a take returned neither a message nor the end")
		}
		for _, m := range msgs {
			got = append(got, string(m))
		}
	}
}

func inOrder(t *testing.T, got []string, n int) {
	t.Helper()
	if len(got) != n {
		t.Fatalf("%d messages, want %d", len(got), n)
	}
	for i, m := range got {
		if m != strconv.Itoa(i) {
			t.Fatalf("message %d is %q", i, m)
		}
	}
}

// Every message comes once and in order, and the end comes after the last
// one, to every caller that asks.
func TestReadAheadKeepsOrderAndTheEnd(t *testing.T) {
	const n = 1000
	s, _ := newSource(t, n)
	close(s.feed)
	r := NewReadAhead(s.ctx, s.recv)
	got, err := takeAll(t, r)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("ended with %v", err)
	}
	inOrder(t, got, n)
	if _, err := r.Take(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("a second caller got %v", err)
	}
}

// With nobody taking, reading stops at the bound, by count and by size.
func TestReadAheadIsBounded(t *testing.T) {
	s, _ := newSource(t, 3*aheadMessages)
	r := NewReadAhead(s.ctx, s.recv)
	until(t, "the bound is reached", r.Full)
	if left := len(s.feed); left != 2*aheadMessages {
		t.Fatalf("%d messages were read ahead, the bound is %d", 3*aheadMessages-left, aheadMessages)
	}
	msgs, err := r.Take(context.Background())
	if err != nil || len(msgs) != aheadMessages {
		t.Fatalf("took %d messages, %v", len(msgs), err)
	}
	until(t, "reading went on after the take", func() bool { return len(s.feed) == aheadMessages })

	big, _ := newSource(t, 0)
	big.feed = make(chan []byte, 4)
	for range 4 {
		big.feed <- make([]byte, aheadBytes/2+1)
	}
	rb := NewReadAhead(big.ctx, big.recv)
	until(t, "the size bound is reached", rb.Full)
	if left := len(big.feed); left != 2 {
		t.Fatalf("%d large messages were read ahead, want 2", 4-left)
	}
}

// A stream that ends while its reader is at the bound still gives what was
// read and then how it ended, and its goroutine ends.
func TestReadAheadStreamEndsAtTheBound(t *testing.T) {
	s, cancel := newSource(t, 2*aheadMessages)
	r := NewReadAhead(s.ctx, s.recv)
	until(t, "the bound is reached", r.Full)
	cancel()
	got, err := takeAll(t, r)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ended with %v", err)
	}
	inOrder(t, got, aheadMessages)
}

// A caller whose own context ends stops waiting, and the stream goes on for
// the next caller.
func TestReadAheadCallerStopsWaiting(t *testing.T) {
	s, _ := newSource(t, 0)
	s.feed = make(chan []byte, 1)
	r := NewReadAhead(s.ctx, s.recv)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Take(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("a caller that stopped waiting got %v", err)
	}
	s.feed <- []byte("0")
	close(s.feed)
	got, err := takeAll(t, r)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("ended with %v", err)
	}
	inOrder(t, got, 1)
}

// A take that does not wait returns nothing while nothing has arrived, and
// Done says when the stream has ended, before its last messages are taken.
func TestReadAheadTakesWithoutWaiting(t *testing.T) {
	s, _ := newSource(t, 0)
	r := NewReadAhead(s.ctx, s.recv)
	if msgs, end := r.TryTake(); len(msgs) != 0 || end != nil {
		t.Fatalf("with nothing sent: %d messages, %v", len(msgs), end)
	}
	s.feed <- []byte("0")
	if err := r.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(s.feed)
	select {
	case <-r.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done is open after the stream's end")
	}
	msgs, end := r.TryTake()
	inOrder(t, []string{string(msgs[0])}, 1)
	if end != nil {
		t.Fatalf("the end came with the last message: %v", end)
	}
	if msgs, end := r.TryTake(); len(msgs) != 0 || !errors.Is(end, io.EOF) {
		t.Fatalf("after the last message: %d messages, %v", len(msgs), end)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := NewReadAhead(s.ctx, func() ([]byte, error) { <-s.ctx.Done(); return nil, s.ctx.Err() }).Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("a wait whose context ended: %v", err)
	}
}
