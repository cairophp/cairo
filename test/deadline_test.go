package test

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

// Middleware::timeout() on one PHP thread: requests stuck in CPU loops (one
// catching every exception, one inside an async() task) are stopped at the
// deadline with 504, and the thread serves the next request right away.
func TestHardDeadlineStopsPHP(t *testing.T) {
	addr := freeAddr()
	cmd := exec.Command(cairoBin, "serve", "../examples/deadline/server.php", "--threads", "1", "--offload-threads", "1")
	cmd.Env = append(os.Environ(), "CAIRO_HTTP="+addr, "CAIRO_TIMEOUT=0.5")
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Signal(os.Interrupt)
		_ = cmd.Wait()
	}()
	if err := waitFor(addr, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	fetch := func(path string) (int, string, time.Duration) {
		start := time.Now()
		resp, err := http.Get("http://" + addr + path)
		if err != nil {
			t.Errorf("GET %s: %v", path, err)
			return 0, "", 0
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), time.Since(start)
	}

	if code, _, _ := fetch("/ok"); code != 200 {
		t.Fatalf("/ok: %d", code)
	}
	start := time.Now()
	var wg sync.WaitGroup
	for _, path := range []string{"/spin", "/spin-catch", "/spin-async"} {
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			if code, body, took := fetch(path); code != 504 || took > 2*time.Second {
				t.Errorf("%s: status %d after %s (%q), want 504 at the deadline", path, code, took, body)
			}
		}(path)
	}
	wg.Wait()

	// The thread was freed: a fast request answers at once.
	if code, _, took := fetch("/ok"); code != 200 || took > 500*time.Millisecond {
		t.Fatalf("/ok after the stuck requests: %d in %s", code, took)
	}
	t.Logf("3 CPU-bound requests stopped at a 0.5s deadline on 1 PHP thread; all answered in %s", time.Since(start).Round(time.Millisecond))

	_, raw, _ := fetch("/stats")
	var seen map[string]any
	if err := json.Unmarshal([]byte(raw), &seen); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	if seen["spin"] != true {
		t.Errorf("/spin did not run its finally block: %s", raw)
	}
	if seen["caught"] != `Cairo\Context\DeadlineExceeded` {
		t.Errorf("/spin-catch caught %v, want Cairo\\Context\\DeadlineExceeded: %s", seen["caught"], raw)
	}
	if n, _ := seen["spin_catch"].(float64); n < 1 {
		t.Errorf("/spin-catch did not catch and carry on before it was stopped: %s", raw)
	}
	if seen["async"] != `Cairo\Context\DeadlineExceeded` {
		t.Errorf("/spin-async: the request saw %v: %s", seen["async"], raw)
	}
}
