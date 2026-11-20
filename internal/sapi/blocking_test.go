package sapi

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/cairophp/cairo/internal/spec"
)

// The detector reads its threshold from its own part of the declaration.
func TestBlockingThreshold(t *testing.T) {
	for name, tc := range map[string]struct {
		section string // what server.php declared; "" for nothing
		ok      bool
		want    time.Duration
	}{
		"nothing declared": {"", true, 0},
		"off":              {`{"after":0}`, true, 0},
		"on":               {`{"after":0.1}`, true, 100 * time.Millisecond},
		"negative":         {`{"after":-1}`, false, 0},
		"not settings":     {`[]`, false, 0},
	} {
		t.Run(name, func(t *testing.T) {
			s := &spec.Spec{}
			if tc.section != "" {
				s.Extensions = map[string]json.RawMessage{"blocking": json.RawMessage(tc.section)}
			}
			got, err := blockingThreshold(s)
			if (err == nil) != tc.ok || got != tc.want {
				t.Fatalf("blockingThreshold: %s and %v, want %s and ok %v", got, err, tc.want, tc.ok)
			}
		})
	}
}

// fakeThreads is an engine whose threads the test sets busy.
type fakeThreads struct {
	stop chan struct{}

	mu          sync.Mutex
	busy        map[int]time.Time
	ticks       int
	interrupted []int
}

func (f *fakeThreads) Stopping() <-chan struct{} { return f.stop }

func (f *fakeThreads) EachBusyThread(fn func(tid int, since time.Time)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ticks++
	for tid, since := range f.busy {
		fn(tid, since)
	}
}

// Called from fn, so with mu held.
func (f *fakeThreads) InterruptThread(tid int) { f.interrupted = append(f.interrupted, tid) }

// waitFor polls until ok says yes of the ticks so far and the threads
// interrupted so far.
func (f *fakeThreads) waitFor(t *testing.T, what string, ok func(ticks int, interrupted []int) bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		f.mu.Lock()
		done := ok(f.ticks, f.interrupted)
		f.mu.Unlock()
		if done {
			return
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	t.Fatalf("%s: after %d ticks the detector interrupted %v", what, f.ticks, f.interrupted)
}

// A thread past the threshold is interrupted once per busy stretch, a thread
// under it never, and the detector's goroutine ends with the engine.
func TestDetectBlocking(t *testing.T) {
	start := time.Now()
	f := &fakeThreads{stop: make(chan struct{}), busy: map[int]time.Time{
		3: start.Add(-time.Minute), // long past the threshold
		5: start.Add(time.Hour),    // not there within this test
	}}
	done := make(chan struct{})
	go func() {
		detectBlocking(f, 20*time.Millisecond)
		close(done)
	}()

	f.waitFor(t, "a busy thread", func(_ int, got []int) bool { return len(got) == 1 && got[0] == 3 })
	f.mu.Lock()
	seen := f.ticks
	f.mu.Unlock()
	f.waitFor(t, "the same stretch, three ticks later", func(ticks int, got []int) bool { return ticks >= seen+3 })
	f.mu.Lock()
	if len(f.interrupted) != 1 {
		t.Errorf("one busy stretch: interrupted %v, want thread 3 once", f.interrupted)
	}
	f.busy[3] = start.Add(-time.Second) // the thread took another event and is busy again
	f.mu.Unlock()
	f.waitFor(t, "its next stretch", func(_ int, got []int) bool { return len(got) == 2 && got[1] == 3 })

	close(f.stop)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the detector is still running after the engine stopped")
	}
}
