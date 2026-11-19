package test

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// startPool runs examples/pool on one PHP thread.
func startPool(t *testing.T) (fetch func(path string, into any) string, addr string) {
	return startPoolThreads(t, 1)
}

func startPoolThreads(t *testing.T, threads int) (fetch func(path string, into any) string, addr string) {
	t.Helper()
	addr = freeAddr()
	cmd := exec.Command(cairoBin, "serve", "../examples/pool/server.php", "--threads", strconv.Itoa(threads))
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
	return func(path string, into any) string {
		t.Helper()
		resp, err := client.Get("http://" + addr + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if into != nil {
			if err := json.Unmarshal(b, into); err != nil {
				t.Fatalf("GET %s: decode %q: %v", path, b, err)
			}
		}
		return strings.TrimSpace(string(b))
	}, addr
}

type poolStats struct {
	Open, Idle, Waiting, Max, Opened, Served int
}

// A fiber waiting for a connection gives up after acquireTimeout, and the
// pool keeps working.
func TestPoolAcquireTimeout(t *testing.T) {
	fetch, _ := startPool(t)
	var got struct {
		Error string
		Took  float64
		Stats poolStats
	}
	fetch("/timeout", &got)
	if !strings.Contains(got.Error, "no connection became free") || got.Took < 0.08 || got.Took > 0.3 {
		t.Fatalf("got %+v, want a PoolTimeout after about 0.1 s", got)
	}
	if got.Stats.Open != 1 || got.Stats.Idle != 1 || got.Stats.Waiting != 0 {
		t.Fatalf("stats %+v, want the one connection idle and nobody waiting", got.Stats)
	}
}

// A request cancelled while it waits for a connection leaves the queue: the
// connection goes to the next request, and the handler of the one that is
// gone does not run on with it.
func TestPoolWaiterLeavesWhenCancelled(t *testing.T) {
	fetch, addr := startPool(t)
	held := make(chan struct{})
	go func() {
		defer close(held)
		resp, err := http.Get("http://" + addr + "/hold?s=0.6")
		if err == nil {
			resp.Body.Close()
		}
	}()
	var stats poolStats
	deadline := time.Now().Add(3 * time.Second)
	for fetch("/stats", &stats); stats.Open != 1; fetch("/stats", &stats) {
		if time.Now().After(deadline) {
			t.Fatal("/hold never took the connection")
		}
		time.Sleep(10 * time.Millisecond)
	}
	impatient := &http.Client{Timeout: 150 * time.Millisecond}
	if _, err := impatient.Get("http://" + addr + "/quick"); err == nil {
		t.Fatal("/quick answered while the only connection was held")
	}
	<-held
	if body := fetch("/quick", nil); !strings.HasPrefix(body, "conn ") {
		t.Fatalf("/quick after the holder finished: %q", body)
	}
	fetch("/stats", &stats)
	if stats.Open != 1 || stats.Idle != 1 || stats.Waiting != 0 || stats.Opened != 1 {
		t.Fatalf("stats %+v, want one connection, idle, and nobody waiting", stats)
	}
	if stats.Served != 1 {
		t.Fatalf("%d requests were served the connection, want 1: the cancelled one must not get it", stats.Served)
	}
}

func TestPoolResetsConnections(t *testing.T) {
	fetch, _ := startPool(t)
	var tx struct {
		InTransaction bool `json:"in_transaction"`
		Rows          int
	}
	fetch("/reset", &tx)
	if tx.InTransaction || tx.Rows != 0 {
		t.Fatalf("got %+v, want the open transaction rolled back", tx)
	}
	var custom struct {
		Reused         bool
		Resets, Opened int
		Stats          poolStats
	}
	fetch("/reset-custom", &custom)
	if !custom.Reused || custom.Resets != 3 || custom.Opened != 2 || custom.Stats.Open != 1 {
		t.Fatalf("got %+v, want 3 resets and the dirty connection replaced", custom)
	}
}

// PDO::inTransaction() on pgsql follows transactions opened with plain SQL,
// through a failed statement, and the pool rolls them back.
func TestPoolResetsPgsqlTransaction(t *testing.T) {
	pgsqlEnv(t)
	fetch, _ := startPool(t)
	var got struct{ Start, Open, Failed, Next, Works bool }
	fetch("/reset-pgsql", &got)
	if got.Start || !got.Open || !got.Failed || got.Next || !got.Works {
		t.Fatalf("got %+v, want a transaction seen while open and failed, then rolled back", got)
	}
}

func TestPoolRetiresOldConnections(t *testing.T) {
	fetch, _ := startPool(t)
	for _, by := range []string{"lifetime", "idle"} {
		var got struct {
			Before, After int
			Stats         poolStats
		}
		fetch("/lifetime?by="+by, &got)
		if got.Before != 1 || got.After != 2 || got.Stats.Open != 1 {
			t.Fatalf("%s: got %+v, want one connection reused, then replaced when too old", by, got)
		}
	}
}

// cairo_pool=2 in a pgsql DSN: PDO objects on every thread share 2 server
// connections. 8 requests that each hold one for 0.3 s run 2 at a time, a
// waiting request suspends only its fiber, and a transaction left open is
// rolled back before the connection is reused.
func TestPoolPgsqlShared(t *testing.T) {
	pgsqlEnv(t)
	fetch, _ := startPoolThreads(t, 4)
	type reply struct {
		Pid int
		App string
	}
	const requests, hold, size = 8, 0.3, 2
	start := time.Now()
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		pids = map[int]bool{}
	)
	for range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var r reply
			fetch("/pgsql-shared?s="+strconv.FormatFloat(hold, 'f', -1, 64), &r)
			mu.Lock()
			pids[r.Pid] = true
			mu.Unlock()
		}()
	}
	time.Sleep(100 * time.Millisecond)
	quick := time.Now()
	fetch("/stats", nil)
	if took := time.Since(quick); took > 200*time.Millisecond {
		t.Errorf("/stats took %v while requests waited for a connection: a wait blocked a thread", took)
	}
	wg.Wait()
	took := time.Since(start).Seconds()
	want := float64(requests/size) * hold
	if took < want*0.95 || took > want+1 {
		t.Errorf("%d requests took %.2f s, want about %.1f s: %d at a time", requests, took, want, size)
	}
	if len(pids) > size {
		t.Errorf("requests ran on %d server connections, want at most %d", len(pids), size)
	}
	for range 2 * size {
		fetch("/pgsql-shared?open=1", nil)
	}
	for range 2 * size {
		var r reply
		fetch("/pgsql-shared", &r)
		if r.App == "left open" {
			t.Fatal("a transaction left open reached the next request")
		}
	}
}
