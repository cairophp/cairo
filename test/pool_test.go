package test

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
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
