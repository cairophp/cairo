package test

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// startRevolt runs examples/revolt (Amp and Revolt on Cairo's driver) on one
// PHP thread, after installing its Composer packages.
func startRevolt(t *testing.T) func(path string) (int, string) {
	t.Helper()
	dir := "../examples/revolt"
	if _, err := os.Stat(dir + "/vendor/autoload.php"); err != nil {
		composer, err := exec.LookPath("composer")
		if err != nil {
			t.Skip("composer is not installed")
		}
		install := exec.Command(composer, "install", "--no-interaction", "--no-progress")
		install.Dir = dir
		if out, err := install.CombinedOutput(); err != nil {
			t.Skipf("composer install failed (offline?): %v\n%s", err, out)
		}
	}
	addr := freeAddr()
	cmd := exec.Command(cairoBin, "serve", "server.php", "--threads", "1", "--offload-threads", "1")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CAIRO_HTTP="+addr)
	var stderr syncBuffer
	cmd.Stdout, cmd.Stderr = os.Stderr, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("server log:\n%s", stderr.String())
		}
	})
	if err := waitFor(addr, 10*time.Second); err != nil {
		t.Fatalf("%v\n%s", err, stderr.String())
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

// Amp's futures, delays and cancellations, and Revolt's callbacks and
// suspensions, run on Cairo's scheduler.
func TestRevoltDriver(t *testing.T) {
	fetch := startRevolt(t)

	var parallel struct {
		Results []string `json:"results"`
		Took    float64  `json:"took"`
	}
	code, body := fetch("/amp/parallel")
	if err := json.Unmarshal([]byte(body), &parallel); code != 200 || err != nil {
		t.Fatalf("/amp/parallel: %d %q %v", code, body, err)
	}
	if strings.Join(parallel.Results, "") != "abc" || parallel.Took < 0.9 || parallel.Took > 1.5 {
		t.Errorf("/amp/parallel: three 1s tasks gave %v in %.2fs, want 3 results in about 1s", parallel.Results, parallel.Took)
	}

	var timeout struct {
		Outcome string  `json:"outcome"`
		Took    float64 `json:"took"`
	}
	code, body = fetch("/amp/timeout")
	if err := json.Unmarshal([]byte(body), &timeout); code != 200 || err != nil || timeout.Outcome != "canceled" || timeout.Took > 1 {
		t.Errorf("/amp/timeout: %d %q, want canceled after 0.2s", code, body)
	}

	if code, body := fetch("/amp/deferred"); code != 200 || body != "done" {
		t.Errorf("/amp/deferred: %d %q", code, body)
	}

	code, body = fetch("/revolt/order")
	var order []string
	if err := json.Unmarshal([]byte(body), &order); code != 200 || err != nil {
		t.Fatalf("/revolt/order: %d %q %v", code, body, err)
	}
	if order[0] != "sync" || strings.Join(order[len(order)-1:], "") != "re-enabled" ||
		strings.Count(body, "repeat") != 3 || strings.Contains(body, "canceled timer ran") ||
		!strings.Contains(body, "defer") || !strings.Contains(body, "queue") || !strings.Contains(body, `"delay"`) {
		t.Errorf("/revolt/order: %s", body)
	}

	if code, body := fetch("/revolt/stream"); code != 200 || body != "hello over a socket" {
		t.Errorf("/revolt/stream: %d %q", code, body)
	}
	if code, body := fetch("/revolt/error"); code != 200 || body != "thrown in a callback" {
		t.Errorf("/revolt/error: %d %q", code, body)
	}

	// amphp/http-client: sockets, connects and stream watchers, end to end.
	code, body = fetch("/amp/http")
	if code == 501 {
		t.Logf("/amp/http skipped: %s", body)
		return
	}
	var client struct {
		Bodies string  `json:"bodies"`
		Took   float64 `json:"took"`
	}
	if err := json.Unmarshal([]byte(body), &client); code != 200 || err != nil {
		t.Fatalf("/amp/http: %d %q %v", code, body, err)
	}
	if client.Bodies != strings.Repeat("ok", 20) || client.Took > 2 {
		t.Errorf("/amp/http: 20 Amp HTTP requests to a 1s endpoint gave %q in %.2fs", client.Bodies, client.Took)
	}
	t.Logf("20 amphp/http-client requests to a 1s endpoint, on 1 PHP thread: %.2fs", client.Took)
}

// Amp\delay() waits in Go: 50 requests sleeping 1s on one PHP thread take
// about 1s, not 50.
func TestRevoltConcurrency(t *testing.T) {
	fetch := startRevolt(t)
	start := time.Now()
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if code, body := fetch("/amp/delay?s=1"); code != 200 || body != "ok" {
				t.Errorf("/amp/delay: %d %q", code, body)
			}
		}()
	}
	wg.Wait()
	took := time.Since(start)
	if took > 3*time.Second {
		t.Fatalf("50 Amp delays of 1s on 1 PHP thread took %s", took)
	}
	t.Logf("50 Amp\\delay(1) requests, on 1 PHP thread: %s", took.Round(time.Millisecond))
}
