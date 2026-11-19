package test

import (
	"io"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

// In isolated mode requests wait in one queue for a free thread: a slow
// request holds one thread, and the requests after it run on the other
// instead of queuing behind it.
func TestIsolatedModeQueuesForFreeThread(t *testing.T) {
	addr := freeAddr()
	cmd := exec.Command(cairoBin, "serve", "../examples/isolated/server.php", "--threads", "2", "--offload-threads", "1")
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
	get := func(ms string) time.Duration {
		start := time.Now()
		resp, err := http.Get("http://" + addr + "/hold?ms=" + ms)
		if err != nil {
			t.Error(err)
			return 0
		}
		defer resp.Body.Close()
		if b, _ := io.ReadAll(resp.Body); string(b) != "held" {
			t.Errorf("got %q", b)
		}
		return time.Since(start)
	}

	slow := make(chan time.Duration, 1)
	go func() { slow <- get("2000") }()
	time.Sleep(100 * time.Millisecond) // let the slow request take its thread first

	const n = 6
	var wg sync.WaitGroup
	took := make([]time.Duration, n)
	for i := range took {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			took[i] = get("50")
		}(i)
	}
	wg.Wait()
	var worst time.Duration
	for _, d := range took {
		worst = max(worst, d)
	}
	t.Logf("%d requests of 50 ms next to one of 2 s, on 2 threads: slowest took %s", n, worst.Round(time.Millisecond))
	if worst > time.Second {
		t.Fatalf("a short request took %s: it queued behind the slow one while a thread was free", worst)
	}
	if d := <-slow; d < 2*time.Second {
		t.Fatalf("slow request took %s", d)
	}
}
