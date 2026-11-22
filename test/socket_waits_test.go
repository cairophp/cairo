package test

import (
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

// serveConns answers each connection to the address it returns with serve.
func serveConns(t *testing.T, serve func(c *net.TCPConn)) string {
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
				serve(c.(*net.TCPConn))
			}()
		}
	}()
	return ln.Addr().String()
}

// pongAfter serves connections that send a number of seconds on a line and
// get "pong" back after that long, as often as they ask.
func pongAfter(t *testing.T) string {
	t.Helper()
	return serveConns(t, func(c *net.TCPConn) {
		var secs float64
		for {
			if _, err := fmt.Fscanln(c, &secs); err != nil {
				return
			}
			time.Sleep(time.Duration(secs * float64(time.Second)))
			fmt.Fprintln(c, "pong")
		}
	})
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
	start := time.Now()
	out := getJSON(t, "/sockets/beside?addr="+pongAfter(t))
	call, _ := out["slowest_call"].(float64)
	slept, _ := out["slept"].(float64)
	if call > 0.3 || slept < 0.04 || slept > 0.3 {
		t.Errorf("next to a 1s wait on a socket, a call through Go took up to %vs and a 50ms sleep %vs", call, slept)
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

// A write that fills the socket's buffer waits for room in its fiber only:
// all of it arrives, and a sleep of another fiber takes its own time.
func TestSocketWriteWaitsForRoom(t *testing.T) {
	addr := serveConns(t, func(c *net.TCPConn) {
		time.Sleep(300 * time.Millisecond)
		n, _ := io.Copy(io.Discard, c)
		fmt.Fprintln(c, n)
	})
	out := getJSON(t, "/sockets/write?addr="+addr)
	if out["written"] != float64(8<<20) || out["got"] != float64(8<<20) {
		t.Errorf("a write of 8 MiB to a server that reads late: %v", out)
	}
	if slept, _ := out["slept"].(float64); slept < 0.04 || slept > 0.25 {
		t.Errorf("next to the write, a 50ms sleep took %vs", slept)
	}
}

// The end of a stream is read as the end, at once.
func TestSocketReadFindsTheEnd(t *testing.T) {
	addr := serveConns(t, func(c *net.TCPConn) { fmt.Fprintln(c, "bye") })
	out := getJSON(t, "/sockets/end?addr="+addr)
	if took, _ := out["took"].(float64); out["line"] != "bye\n" || out["after"] != false || out["eof"] != true || took > 0.2 {
		t.Errorf("reading past the last line of a server that closed: %v", out)
	}
}
