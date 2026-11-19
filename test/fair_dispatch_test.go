package test

import (
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"sync"
	"testing"
	"time"
)

// Two PHP threads; one runs a 300 ms CPU loop. Light requests sent
// meanwhile go to the other thread instead of waiting behind the loop.
func TestFairDispatch(t *testing.T) {
	addr := freeAddr()
	cmd := exec.Command(cairoBin, "serve", "../examples/fair/server.php", "--threads", "2", "--offload-threads", "1")
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
	get := func(path string) time.Duration {
		start := time.Now()
		resp, err := http.Get("http://" + addr + path)
		if err != nil {
			t.Error(err)
			return 0
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return time.Since(start)
	}
	for range 5 {
		get("/light") // warm both threads
	}

	spun := make(chan struct{})
	go func() { get("/spin?ms=300"); close(spun) }()
	time.Sleep(50 * time.Millisecond) // the loop is running now

	var mu sync.Mutex
	var took []time.Duration
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				d := get("/light")
				mu.Lock()
				took = append(took, d)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	<-spun
	sort.Slice(took, func(i, j int) bool { return took[i] < took[j] })
	slowest := took[len(took)-1]
	t.Logf("40 light requests during a 300 ms CPU loop on 2 threads: median %s, slowest %s",
		took[len(took)/2].Round(time.Millisecond), slowest.Round(time.Millisecond))
	if slowest > 150*time.Millisecond {
		t.Errorf("a light request waited %s behind the CPU loop", slowest.Round(time.Millisecond))
	}
}
