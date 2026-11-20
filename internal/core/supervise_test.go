package core

import "testing"

// A thread that dies answers for the calls it took. Those still queued wait
// for its replacement, unless the thread was retiring: then nothing will
// take them, and they are answered too.
func TestFailCalls(t *testing.T) {
	for _, tc := range []struct {
		name   string
		queued bool
		want   int
	}{{"crashed", false, 1}, {"retiring", true, 2}} {
		t.Run(tc.name, func(t *testing.T) {
			e := &Engine{}
			th := e.newThread(0, poolWorker, 0)
			taken := &call{id: 1, thread: th, resp: make(chan *reply, 1)}
			waiting := &call{id: 2, thread: th, resp: make(chan *reply, 1)}
			th.calls[taken.id] = taken
			th.inbox <- event{call: waiting}
			th.inflight.Store(2)

			if n := e.failCalls(th, tc.queued); n != tc.want {
				t.Errorf("failed %d calls, want %d", n, tc.want)
			}
			if len(taken.resp) != 1 || <-taken.resp != nil {
				t.Error("the call the thread took was not answered with nil")
			}
			if len(waiting.resp) == 1 != tc.queued {
				t.Errorf("the queued call was answered: %v, want %v", !tc.queued, tc.queued)
			}
			if left := th.inflight.Load(); left != int64(2-tc.want) {
				t.Errorf("%d calls in flight, want %d", left, 2-tc.want)
			}
			if len(th.calls) != 0 {
				t.Errorf("%d calls left in the thread's table", len(th.calls))
			}
		})
	}
}
