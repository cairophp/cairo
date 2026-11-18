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

// startCache runs examples/cache on one PHP thread and returns a GET helper.
func startCache(t *testing.T) func(path string, header ...string) (string, http.Header) {
	t.Helper()
	addr := freeAddr()
	cmd := exec.Command(cairoBin, "serve", "../examples/cache/server.php", "--threads", "1")
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
	client := &http.Client{Timeout: 10 * time.Second}
	return func(path string, header ...string) (string, http.Header) {
		req, _ := http.NewRequest("GET", "http://"+addr+path, nil)
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Errorf("GET %s: %v", path, err)
			return "", nil
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return strings.TrimSpace(string(b)), resp.Header
	}
}

func productsBuild(t *testing.T, body string) float64 {
	t.Helper()
	var v struct{ Build float64 }
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return v.Build
}

// 50 identical requests at once make one PHP call; repeats make none, and a
// stale entry is served without waiting while one request rebuilds it.
func TestResponseCache(t *testing.T) {
	get := startCache(t)
	start := time.Now()
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if body, _ := get("/products?page=2"); productsBuild(t, body) != 1 {
				t.Errorf("got %s, want build 1", body)
			}
		}()
	}
	wg.Wait()
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Errorf("50 merged requests took %v, want about 0.2 s", d)
	}
	get("/products?page=2")
	if calls, _ := get("/calls"); calls != "1" {
		t.Fatalf("PHP ran %s times for 51 identical requests, want 1", calls)
	}

	time.Sleep(1200 * time.Millisecond) // past the 1 s TTL
	start = time.Now()
	if body, _ := get("/products?page=2"); productsBuild(t, body) != 1 {
		t.Fatalf("stale request got %s, want the stale build 1", body)
	}
	if d := time.Since(start); d > 150*time.Millisecond {
		t.Errorf("stale request waited %v for the rebuild", d)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		body, _ := get("/products?page=2")
		if productsBuild(t, body) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the stale entry was never rebuilt")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestResponseCacheVaryAndPrivate(t *testing.T) {
	get := startCache(t)
	for range 3 {
		if en, _ := get("/hello", "Accept-Language", "en"); en != "hello" {
			t.Fatalf("en got %q", en)
		}
		if fr, _ := get("/hello", "Accept-Language", "fr"); fr != "bonjour" {
			t.Fatalf("fr got %q", fr)
		}
	}
	if calls, _ := get("/calls"); calls != "2" {
		t.Fatalf("PHP ran %s times for two languages, want 2", calls)
	}
	_, h1 := get("/session")
	_, h2 := get("/session")
	if h1.Get("Set-Cookie") == "" || h1.Get("Set-Cookie") == h2.Get("Set-Cookie") {
		t.Fatalf("Set-Cookie responses were shared: %q and %q", h1.Get("Set-Cookie"), h2.Get("Set-Cookie"))
	}
	if body, _ := get("/hello", "Cookie", "sid=1"); body != "hello" {
		t.Fatalf("got %q", body)
	}
	if calls, _ := get("/calls"); calls != "5" {
		t.Fatalf("PHP ran %s times, want 5: a request with a cookie skips the cache", calls)
	}
}
