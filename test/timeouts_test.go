package test

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// closedWithin reads conn until the server closes it and fails unless that
// happens between from and to from now.
func closedWithin(t *testing.T, conn net.Conn, from, to time.Duration) {
	t.Helper()
	start := time.Now()
	_ = conn.SetReadDeadline(start.Add(to))
	got, err := io.ReadAll(conn)
	took := time.Since(start)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatalf("the server still held the connection after %v", to)
	}
	if took < from {
		t.Fatalf("the server closed the connection after %v, before %v (read %q, %v)", took, from, got, err)
	}
}

// A client that sends its head slowly, or keeps a connection with nothing to
// ask, loses it to timeouts()'s $header and $idle, while a stream that
// outlasts both is untouched: they cover a request's head and the wait
// between requests, not an answer.
func TestHeaderAndIdleTimeouts(t *testing.T) {
	t.Parallel()
	base := startStreaming(t, "server.php", "CAIRO_HEADER_TIMEOUT=0.5", "CAIRO_IDLE_TIMEOUT=0.5")
	addr := strings.TrimPrefix(base, "http://")

	slow, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()
	if _, err := io.WriteString(slow, "GET /stream HTTP/1.1\r\nHost: test\r\n"); err != nil {
		t.Fatal(err)
	}
	closedWithin(t, slow, 400*time.Millisecond, 3*time.Second)

	kept, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer kept.Close()
	if _, err := io.WriteString(kept, "GET /stream?wait=1 HTTP/1.1\r\nHost: test\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(kept)
	resp, err := http.ReadResponse(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || string(body) != "first\nsecond\n" {
		t.Fatalf("a stream of 1s under 0.5s timeouts: %q, %v", body, err)
	}
	closedWithin(t, kept, 400*time.Millisecond, 3*time.Second)
}

// With no timeouts() at all a slow head is still cut, after 10 s.
func TestHeaderTimeoutByDefault(t *testing.T) {
	t.Parallel()
	addr := freeAddr()
	cmd := exec.Command(cairoBin, "serve", "../examples/public/server.php", "--threads", "1", "--offload-threads", "1")
	cmd.Env = append(os.Environ(), "CAIRO_HTTP="+addr)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		_ = cmd.Wait()
	})
	// Measure this server only: in one CI run a bare dial got through 1 ms
	// after start, far too soon for cairo, and the slow head was cut in 9 us.
	if err := waitForAnswer("http://"+addr+"/", "app /", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	slow, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()
	if _, err := io.WriteString(slow, "GET / HTTP/1.1\r\nHost: test\r\n"); err != nil {
		t.Fatal(err)
	}
	closedWithin(t, slow, 9*time.Second, 14*time.Second)
}

// waitForAnswer polls url until it answers want.
func waitForAnswer(url, want string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var got string
	for time.Now().Before(deadline) {
		if resp, err := http.Get(url); err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if got = string(b); got == want {
				return nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("%s did not answer %q (last %q)", url, want, got)
}
