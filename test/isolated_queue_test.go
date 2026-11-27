package test

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

// A request whose client leaves while it waits for a thread is dropped from
// the queue: it never runs, and the one behind it does not wait for it.
func TestIsolatedQueuedRequestThatLeaves(t *testing.T) {
	addr := freeAddr()
	cmd := exec.Command(cairoBin, "serve", "../examples/isolated/server.php", "--threads", "1", "--offload-threads", "1")
	cmd.Env = append(os.Environ(), "CAIRO_HTTP="+addr)
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
	hold := func(ctx context.Context, ms string) error {
		req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+addr+"/hold?ms="+ms, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, err = io.ReadAll(resp.Body)
		return err
	}

	start := time.Now()
	first := make(chan error, 1)
	go func() { first <- hold(context.Background(), "500") }()
	time.Sleep(100 * time.Millisecond) // it has the thread
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := hold(ctx, "3000"); err == nil {
		t.Fatal("the request that left was answered")
	}
	if err := hold(context.Background(), "0"); err != nil {
		t.Fatal(err)
	}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("the last request took until %s: it waited for the one that left", took.Round(time.Millisecond))
	}
}

// Isolated mode, one thread: maxRequests(5) replaces it again and again
// while requests queue for it, and none fails.
func TestIsolatedRecyclingWhileRequestsQueue(t *testing.T) {
	fetch := startSupervise(t, "CAIRO_CONCURRENCY=isolated", "CAIRO_MAX_REQUESTS=5")
	seen := map[string]bool{}
	var mu sync.Mutex
	for wave := 0; wave < 6; wave++ {
		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				code, id := fetch("/boot")
				if code != 200 {
					t.Errorf("/boot during recycling: status %d (%q)", code, id)
					return
				}
				mu.Lock()
				seen[id] = true
				mu.Unlock()
			}()
		}
		wg.Wait()
	}
	if len(seen) < 3 {
		t.Fatalf("120 requests with maxRequests(5) ran on %d threads, want several", len(seen))
	}
	t.Logf("120 queued requests ran on %d successive threads, none failed", len(seen))
}

// Isolated mode, one thread: the thread dies in a request while others wait
// for it. The one it ran gets a 500, and the others are served by the thread
// that takes its place.
func TestIsolatedCrashWhileRequestsQueue(t *testing.T) {
	fetch := startSupervise(t, "CAIRO_CONCURRENCY=isolated")
	_, first := fetch("/boot")
	crashed := make(chan int, 1)
	go func() { code, _ := fetch("/crash"); crashed <- code }()
	time.Sleep(100 * time.Millisecond) // it has the thread
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if code, id := fetch("/boot"); code != 200 || id == first {
				t.Errorf("/boot queued behind the crash: %d %q (before: %q), want 200 from the new thread", code, id, first)
			}
		}()
	}
	wg.Wait()
	if code := <-crashed; code != 500 {
		t.Errorf("/crash: %d, want 500", code)
	}
}
