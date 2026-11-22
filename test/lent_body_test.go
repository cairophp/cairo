package test

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

// A large answer is written to its client from the memory PHP's thread
// handed over, without a copy in Go, and freed after the write: every client
// gets its own bytes, whatever the others do at the same time.
func TestLargeAnswersStayApart(t *testing.T) {
	var wg sync.WaitGroup
	for i := range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := make([]byte, 40_000+i*37_123) // from just past the size that is lent to 900 KB
			if _, err := rand.Read(body); err != nil {
				t.Error(err)
				return
			}
			for range 4 {
				resp, err := http.Post("http://"+httpAddr+"/echo-body", "application/octet-stream", bytes.NewReader(body))
				if err != nil {
					t.Error(err)
					return
				}
				got, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil || !bytes.Equal(got, body) {
					t.Errorf("%d bytes sent, %d came back (%v), or other bytes", len(body), len(got), err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// A client that goes away in the middle of a large answer: the write fails,
// the answer is freed all the same, and the server goes on.
func TestLargeAnswerToAClientThatLeaves(t *testing.T) {
	body := bytes.Repeat([]byte("0123456789abcdef"), 1<<16) // 1 MiB
	for range 20 {
		conn, err := net.Dial("tcp", httpAddr)
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		fmt.Fprintf(conn, "POST /echo-body HTTP/1.1\r\nHost: x\r\nContent-Length: %d\r\n\r\n", len(body))
		if _, err := conn.Write(body); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(conn, make([]byte, 512)); err != nil {
			t.Fatalf("the answer did not start: %v", err)
		}
		conn.Close()
	}
	if got := get(t, "/hello/after"); got != "Hello after\n" {
		t.Fatalf("after the clients that left: %q", got)
	}
}

// An answer that PHP gives after the request's deadline is dropped, also a
// large one, and the thread serves the next request.
func TestLargeAnswerAfterTheDeadline(t *testing.T) {
	addr := freeAddr()
	cmd := exec.Command(cairoBin, "serve", "../examples/deadline/server.php", "--threads", "1", "--offload-threads", "1")
	cmd.Env = append(os.Environ(), "CAIRO_HTTP="+addr, "CAIRO_TIMEOUT=0.2")
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		_ = cmd.Wait()
	})
	if err := waitFor(addr, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	status := func(path string) int {
		t.Helper()
		resp, err := http.Get("http://" + addr + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}
	for range 5 {
		if code := status("/late"); code != http.StatusGatewayTimeout {
			t.Fatalf("/late: %d, want 504", code)
		}
	}
	if code := status("/ok"); code != http.StatusOK {
		t.Fatalf("/ok after the late answers: %d", code)
	}
}
