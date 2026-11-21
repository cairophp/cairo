package test

import (
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// startSupervise runs examples/supervise on one PHP thread.
func startSupervise(t *testing.T, env ...string) (fetch func(path string) (int, string)) {
	t.Helper()
	addr := freeAddr()
	cmd := exec.Command(cairoBin, "serve", "../examples/supervise/server.php", "--threads", "1", "--offload-threads", "1")
	cmd.Env = append(append(os.Environ(), "CAIRO_HTTP="+addr), env...)
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
	client := &http.Client{Timeout: 10 * time.Second}
	return func(path string) (int, string) {
		resp, err := client.Get("http://" + addr + path)
		if err != nil {
			t.Errorf("GET %s: %v", path, err)
			return 0, ""
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, strings.TrimSpace(string(b))
	}
}

// A fatal error kills the only PHP thread: the requests it held (one waiting
// in its root fiber, one in an async() task) get 500 at once instead of
// hanging, the thread is restarted, and the server keeps answering.
func TestCrashedThreadIsRestarted(t *testing.T) {
	fetch := startSupervise(t)
	code, first := fetch("/boot")
	if code != 200 || first == "" {
		t.Fatalf("/boot: %d %q", code, first)
	}

	var wg sync.WaitGroup
	for _, path := range []string{"/slow", "/slow-async"} {
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			start := time.Now()
			code, body := fetch(path)
			if code != 500 || time.Since(start) > 3*time.Second {
				t.Errorf("%s while the thread crashed: status %d after %s (%q), want 500", path, code, time.Since(start), body)
			}
		}(path)
	}
	time.Sleep(200 * time.Millisecond) // both are waiting in PHP
	if code, body := fetch("/crash"); code != 500 {
		t.Errorf("/crash: status %d (%q), want 500", code, body)
	}
	wg.Wait()

	code, second := fetch("/boot")
	if code != 200 || second == "" || second == first {
		t.Fatalf("/boot after the crash: %d %q (before: %q), want 200 from a new thread", code, second, first)
	}
	if code, body := fetch("/slow-async"); code != 200 || body != second {
		t.Fatalf("/slow-async on the restarted thread: %d %q", code, body)
	}
	t.Logf("thread restarted after a fatal error; in-flight requests got 500")
}

// A fatal error in an offloaded job kills the only offload thread: the fiber
// that waits for the job is told at once instead of waiting forever, the
// thread is restarted and runs the next job, and the request thread never
// stopped.
func TestCrashedOffloadThreadIsRestarted(t *testing.T) {
	fetch := startSupervise(t)
	_, request := fetch("/boot")
	code, first := fetch("/offload/boot")
	if code != 200 || first == "" || first == request {
		t.Fatalf("/offload/boot: %d %q, want 200 from another thread than %q", code, first, request)
	}

	start := time.Now()
	want := `Cairo\OffloadException: the offload thread stopped before the job was done`
	if code, body := fetch("/offload/crash"); code != 200 || body != want || time.Since(start) > 3*time.Second {
		t.Fatalf("/offload/crash: status %d after %s (%q), want %q", code, time.Since(start), body, want)
	}

	code, second := fetch("/offload/boot")
	if code != 200 || second == "" || second == first {
		t.Fatalf("/offload/boot after the crash: %d %q (before: %q), want 200 from a new thread", code, second, first)
	}
	if _, now := fetch("/boot"); now != request {
		t.Fatalf("/boot after the crash: %q, want the same request thread, %q", now, request)
	}
}

// maxRequests(5): the thread is replaced again and again under concurrent
// load, and no request fails while it happens.
func TestMaxRequestsRecyclesThread(t *testing.T) {
	fetch := startSupervise(t, "CAIRO_MAX_REQUESTS=5")

	// One at a time: the first five run on the first thread.
	var ids []string
	for i := 0; i < 6; i++ {
		code, id := fetch("/boot")
		if code != 200 {
			t.Fatalf("/boot #%d: %d", i+1, code)
		}
		ids = append(ids, id)
	}
	for i := 1; i < 5; i++ {
		if ids[i] != ids[0] {
			t.Fatalf("thread changed before 5 requests: %v", ids)
		}
	}

	seen := map[string]bool{}
	var mu sync.Mutex
	for wave := 0; wave < 6; wave++ {
		var wg sync.WaitGroup
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func(path string) {
				defer wg.Done()
				code, id := fetch(path)
				if code != 200 {
					t.Errorf("%s during recycling: status %d (%q)", path, code, id)
					return
				}
				mu.Lock()
				seen[id] = true
				mu.Unlock()
			}([]string{"/boot", "/slow", "/slow-async"}[i%3])
		}
		wg.Wait()
	}
	if len(seen) < 3 {
		t.Fatalf("60 requests with maxRequests(5) ran on %d threads, want several", len(seen))
	}
	t.Logf("60 concurrent requests ran on %d successive threads, none failed", len(seen))
}

// maxMemory: a thread whose PHP memory grows past the limit is replaced.
func TestMaxMemoryRecyclesThread(t *testing.T) {
	fetch := startSupervise(t, "CAIRO_MAX_MEMORY=24M")
	seen := map[string]bool{}
	for i := 0; i < 12; i++ { // 4 MiB more per request
		code, id := fetch("/grow")
		if code != 200 {
			t.Fatalf("/grow #%d: %d (%q)", i+1, code, id)
		}
		seen[id] = true
	}
	if len(seen) < 2 {
		t.Fatalf("memory grew by 48 MiB with maxMemory('24M') and the thread was never replaced")
	}
}

// maxInFlight(5) on one thread: 10 requests that each wait 1s run five at a
// time, so they take about 2s; the rest wait in Go, and all succeed.
func TestMaxInFlightQueuesInGo(t *testing.T) {
	fetch := startSupervise(t, "CAIRO_MAX_IN_FLIGHT=5")
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if code, body := fetch("/slow"); code != 200 {
				t.Errorf("/slow: %d %q", code, body)
			}
		}()
	}
	wg.Wait()
	took := time.Since(start)
	if took < 1900*time.Millisecond || took > 3500*time.Millisecond {
		t.Fatalf("10 requests of 1s with maxInFlight(5) on 1 thread took %s, want about 2s", took)
	}
	t.Logf("10 requests of 1s, 5 in flight at a time: %s", took.Round(time.Millisecond))
}
