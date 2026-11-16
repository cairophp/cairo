package test

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// App::detectBlocking(): a request that keeps its PHP thread busy past the
// threshold is logged once, naming the line it was on; one that yields to Go
// (Cairo\sleep, or a hooked usleep) is not.
func TestBlockingDetector(t *testing.T) {
	src, err := os.ReadFile("../examples/blocking/server.php")
	if err != nil {
		t.Fatal(err)
	}
	line := 0
	for i, l := range strings.Split(string(src), "\n") {
		if strings.Contains(l, "BLOCKING-LINE") {
			line = i + 1
		}
	}

	addr := freeAddr()
	cmd := exec.Command(cairoBin, "serve", "../examples/blocking/server.php", "--threads", "1", "--offload-threads", "1")
	cmd.Env = append(os.Environ(), "CAIRO_HTTP="+addr, "CAIRO_DETECT_BLOCKING=0.1")
	var stderr syncBuffer
	cmd.Stdout, cmd.Stderr = os.Stderr, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Signal(os.Interrupt)
		_ = cmd.Wait()
	}()
	if err := waitFor(addr, 10*time.Second); err != nil {
		t.Fatalf("%v\n%s", err, stderr.String())
	}
	fetch := func(path string) {
		resp, err := http.Get("http://" + addr + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s: status %d", path, resp.StatusCode)
		}
	}
	reports := func() int { return strings.Count(stderr.String(), "cairo: blocking") }

	fetch("/ok")
	fetch("/usleep")
	time.Sleep(200 * time.Millisecond)
	if n := reports(); n != 0 {
		t.Fatalf("/ok and /usleep yield to Go but were reported as blocking:\n%s", stderr.String())
	}

	fetch("/spin")
	deadline := time.Now().Add(2 * time.Second)
	for reports() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	logs := stderr.String()
	if n := reports(); n != 1 {
		t.Fatalf("/spin: %d blocking reports, want 1:\n%s", n, logs)
	}
	if want := fmt.Sprintf("server.php:%d", line); !strings.Contains(logs, want) {
		t.Fatalf("blocking report does not name %s:\n%s", want, logs)
	}

	fetch("/hash")
	deadline = time.Now().Add(2 * time.Second)
	for reports() < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := reports(); n != 2 {
		t.Fatalf("/hash: %d blocking reports in all, want 2:\n%s", n, stderr.String())
	}
	t.Logf("blocking report:\n%s", logs[strings.Index(logs, "cairo: blocking"):])
}
