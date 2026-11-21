package test

import (
	"fmt"
	"net"
	"net/url"
	"testing"
	"time"
)

// pongAfter serves connections that send a number of seconds on a line and
// get "pong" back after that long, as often as they ask.
func pongAfter(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				var secs float64
				for {
					if _, err := fmt.Fscanln(c, &secs); err != nil {
						return
					}
					time.Sleep(time.Duration(secs * float64(time.Second)))
					fmt.Fprintln(c, "pong")
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// A read of a socket waits no longer than the stream's timeout, and the
// socket can be waited on again afterwards.
func TestSocketWaitEndsAtTheStreamTimeout(t *testing.T) {
	out := getJSON(t, "/sockets/timeout?addr="+pongAfter(t))
	took, _ := out["took"].(float64)
	if out["first"] != false || out["timed_out"] != true || took < 0.1 || took > 0.35 {
		t.Errorf("a read with a 0.15s timeout of an answer that takes 0.4s: %v", out)
	}
	if out["second"] != "pong" {
		t.Errorf("the next read of the socket got %q, want the answer", out["second"])
	}
}

// A read of a socket ends with the context it runs under, does not start
// under one that has ended, and leaves the socket usable.
func TestSocketWaitEndsWithItsContext(t *testing.T) {
	out := getJSON(t, "/sockets/context?addr="+pongAfter(t))
	for _, tc := range []struct {
		key, how string
		min, max float64
	}{
		{"deadline", `Cairo\Context\DeadlineExceeded`, 0.1, 0.35},
		{"canceled", `Cairo\Context\Canceled`, 0.05, 0.35},
		{"ended", `Cairo\Context\Canceled`, 0, 0.05},
		{"live", "pong", 0.35, 1},
	} {
		got, _ := out[tc.key].([]any)
		if len(got) != 3 {
			t.Fatalf("%s: %v", tc.key, out)
		}
		if took, _ := got[1].(float64); got[0] != tc.how || took < tc.min || took > tc.max {
			t.Errorf("%s: a read of an answer that takes 0.4s ended as %v after %vs, want %s within %v to %vs", tc.key, got[0], got[1], tc.how, tc.min, tc.max)
		}
		if tc.key != "live" && got[2] != "pong" {
			t.Errorf("%s: the next read of the socket got %q, want the answer", tc.key, got[2])
		}
	}
}

// A thread that sleeps until a socket is ready still wakes for what Go has
// for it and for its timers: next to a fiber that waits a second for a
// socket, Go operations and a sleep take their own time.
func TestSocketWaitLeavesTheThreadAwake(t *testing.T) {
	q := url.Values{"addr": {pongAfter(t)}, "url": {"http://" + httpAddr + "/ctx/value"}}
	start := time.Now()
	out := getJSON(t, "/sockets/beside?"+q.Encode())
	call, _ := out["slowest_call"].(float64)
	slept, _ := out["slept"].(float64)
	if call > 0.3 || slept < 0.04 || slept > 0.3 {
		t.Errorf("next to a 1s wait on a socket, an HTTP call took up to %vs and a 50ms sleep %vs", call, slept)
	}
	if took := time.Since(start); out["read"] != "pong" || took < 900*time.Millisecond || took > 3*time.Second {
		t.Errorf("the socket's fiber read %q, and the request took %s; want the answer after about 1s", out["read"], took.Round(time.Millisecond))
	}
}

// One fiber waits on a socket at a time: a second one is told so.
func TestSocketWaitIsOneFibersAtATime(t *testing.T) {
	out := getJSON(t, "/sockets/shared?addr="+pongAfter(t))
	if out["first"] != "pong" || out["second"] != "socket is already in use by another fiber" {
		t.Errorf("two fibers reading one socket: %v", out)
	}
}
