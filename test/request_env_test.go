package test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

// envAnswer is what examples/request-local answers at /env.
type envAnswer struct {
	Before, After map[string]any
	Listed        any
}

func getEnv(name, addr string) (envAnswer, error) {
	var a envAnswer
	resp, err := http.Get(fmt.Sprintf("http://%s/env?name=%s", addr, name))
	if err != nil {
		return a, err
	}
	defer resp.Body.Close()
	return a, json.NewDecoder(resp.Body).Decode(&a)
}

// checkEnv fails unless request name started from server.php's environment
// alone and saw its own putenv() to the end.
func checkEnv(t *testing.T, name string, a envAnswer) {
	t.Helper()
	if a.Before["tenant"] != false || a.Before["booted"] != "yes" {
		t.Errorf("%s started with %v, want no TENANT and BOOTED=yes", name, a.Before)
	}
	if a.After["tenant"] != name || a.After["booted"] != false || a.Listed != name {
		t.Errorf("%s ended with %v (listed %v), want its own TENANT and no BOOTED", name, a.After, a.Listed)
	}
}

func startRequestLocal(t *testing.T, mode string) string {
	t.Helper()
	addr := freeAddr()
	cmd := exec.Command(cairoBin, "serve", "../examples/request-local/server.php", "--threads", "1")
	cmd.Env = append(os.Environ(), "CAIRO_HTTP="+addr, "CAIRO_MODE="+mode)
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
	return addr
}

// putenv() in a request is that request's, in fiber mode too, where 50
// requests that each set it and wait 200 ms run side by side on ONE PHP
// thread; what server.php set is where every request starts.
func TestPutenvIsTheRequestsInFiberMode(t *testing.T) {
	addr := startRequestLocal(t, "fibers")
	const n = 50
	answers := make([]envAnswer, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := time.Now()
	for i := range n {
		wg.Go(func() { answers[i], errs[i] = getEnv(fmt.Sprintf("t%d", i), addr) })
	}
	wg.Wait()
	if took := time.Since(start); took > 1500*time.Millisecond {
		t.Errorf("%d requests that each wait 200 ms took %v on one PHP thread", n, took)
	}
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("request %d: %v", i, errs[i])
		}
		checkEnv(t, fmt.Sprintf("t%d", i), answers[i])
	}
	a, err := getEnv("last", addr)
	if err != nil {
		t.Fatal(err)
	}
	checkEnv(t, "last", a)
}

// In isolated mode the thread's one PHP request serves request after
// request: none of them sees what the one before set.
func TestPutenvIsTheRequestsInIsolatedMode(t *testing.T) {
	addr := startRequestLocal(t, "isolated")
	for i := range 3 {
		name := fmt.Sprintf("i%d", i)
		a, err := getEnv(name, addr)
		if err != nil {
			t.Fatal(err)
		}
		checkEnv(t, name, a)
	}
}
