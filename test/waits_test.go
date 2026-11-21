package test

import (
	"reflect"
	"testing"
	"time"
)

// Sleeps are timers of the PHP thread: tasks wake in the order of their
// times, not of their start, and the longest one decides how long all take.
func TestSleepsWakeInOrder(t *testing.T) {
	out := getJSON(t, "/waits/order")
	want := []any{0.0, 10.0, 20.0, 30.0, 40.0, 50.0, 60.0, 70.0, 80.0, 90.0}
	took, _ := out["took"].(float64)
	if !reflect.DeepEqual(out["woke"], want) || took < 0.09 || took > 0.5 {
		t.Errorf("ten tasks sleeping 0 to 90 ms woke as %v in %.3f s, want %v in about 0.09 s", out["woke"], took, want)
	}
}

// Three hundred tasks that sleep at once, more than the thread's lists of
// fibers hold at first, wake in the order they went to sleep. A fiber whose
// task is done runs another one: 64 wait for that, the others end.
func TestBurstOfSleepsWakesInOrderOnReusedFibers(t *testing.T) {
	out := getJSON(t, "/waits/burst?n=300")
	if out["inOrder"] != true || out["reused"] != 64.0 {
		t.Errorf("two rounds of 300 sleeps: in order %v, %v fibers ran a task of both rounds; want true and 64", out["inOrder"], out["reused"])
	}
}

// Two fibers that keep waking each other never leave the thread idle. It
// still takes what Go sends: other requests are answered while they run.
func TestBusyFibersLeaveRoomForRequests(t *testing.T) {
	done := make(chan map[string]any, 1)
	go func() { done <- getJSON(t, "/waits/ping-pong?ms=1500") }()
	var out map[string]any
	answered := 0
	for out == nil {
		start := time.Now()
		get(t, "/ctx/value")
		if took := time.Since(start); took > 500*time.Millisecond {
			t.Fatalf("a request next to two busy fibers took %s", took.Round(time.Millisecond))
		}
		answered++
		select {
		case out = <-done:
		default:
		}
	}
	if answered < 20 {
		t.Errorf("%d requests were answered while the two fibers ran for 1.5 s", answered)
	}
	// Every round left the timer of a wait that ended early. They are
	// dropped as they pile up, not kept for the hour they were set for.
	rounds, _ := out["rounds"].(float64)
	grew, _ := out["grew"].(float64)
	if rounds < 10000 || grew > 256<<10 {
		t.Errorf("%.0f rounds of a wait with a timeout that never passed grew memory by %.0f bytes", rounds, grew)
	}
	t.Logf("%.0f rounds in 1.5 s next to %d requests, %.0f bytes kept", rounds, answered, grew)
}
