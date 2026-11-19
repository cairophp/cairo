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

// examples/request-local: 50 requests on ONE PHP thread each set
// Tenant::$current, wait 200 ms while the others run, and read it back
// from an async() task. Each must see its own value, start from the boot
// value, and the 50 must still run side by side.
func TestRequestLocalStatics(t *testing.T) {
	addr := freeAddr()
	cmd := exec.Command(cairoBin, "serve", "../examples/request-local/server.php", "--threads", "1")
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

	const n = 50
	type result struct {
		Before, After string
		Seen          []string
	}
	results := make([]result, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := time.Now()
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(fmt.Sprintf("http://%s/tenant?name=t%d", addr, i))
			if err != nil {
				errs[i] = err
				return
			}
			defer resp.Body.Close()
			errs[i] = json.NewDecoder(resp.Body).Decode(&results[i])
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	for i, r := range results {
		if errs[i] != nil {
			t.Fatalf("request %d: %v", i, errs[i])
		}
		want := fmt.Sprintf("t%d", i)
		if r.Before != "boot" || r.After != want || len(r.Seen) != 1 || r.Seen[0] != want {
			t.Errorf("request %d: %+v, want before=boot after=%s seen=[%s]", i, r, want, want)
		}
	}
	t.Logf("%d requests with request-local statics, each waiting 200 ms, on 1 PHP thread: %s", n, elapsed.Round(time.Millisecond))
	if elapsed > time.Second {
		t.Errorf("took %s: the requests did not run side by side", elapsed)
	}
}
