package test

import (
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// examples/heavy names GET /heavy, which runs 80 ms of PHP in one stretch,
// once a minute however often it runs, and never the same work split by
// waits (/yields) or moved to offload() (/offloaded).
func TestHeavyRoutes(t *testing.T) {
	addr := freeAddr()
	cmd := exec.Command(cairoBin, "serve", "../examples/heavy/server.php", "--threads", "1", "--offload-threads", "1")
	cmd.Env = append(os.Environ(), "CAIRO_HTTP="+addr)
	var logs syncBuffer
	cmd.Stdout, cmd.Stderr = os.Stderr, &logs
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
	for _, path := range []string{"/yields", "/offloaded", "/heavy", "/heavy"} {
		resp, err := http.Get("http://" + addr + path)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s: status %d\n%s", path, resp.StatusCode, logs.String())
		}
	}

	var lines []string
	for deadline := time.Now().Add(2 * time.Second); len(lines) == 0 && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		for _, l := range strings.Split(logs.String(), "\n") {
			if strings.Contains(l, `msg="heavy route`) {
				lines = append(lines, l)
			}
		}
	}
	if len(lines) != 1 || !strings.Contains(lines[0], `route="GET /heavy"`) {
		t.Fatalf("want one heavy route line, for GET /heavy:\n%s", logs.String())
	}
	t.Log(lines[0])
	m := regexp.MustCompile(` longest_run_ms=(\d+)`).FindStringSubmatch(lines[0])
	if m == nil {
		t.Fatalf("no longest_run_ms in %s", lines[0])
	}
	if ms, _ := strconv.Atoi(m[1]); ms < 70 || ms > 300 {
		t.Errorf("longest_run_ms = %d, want about 80", ms)
	}
}
