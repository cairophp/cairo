package test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// examples/slow logs GET /report (an HTTP call, three sleeps in async()
// tasks, one of its own and 30 ms of PHP) with that breakdown, and never
// logs GET /fast.
func TestSlowRequests(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		fmt.Fprint(w, "ok")
	}))
	defer upstream.Close()

	addr, logs := startSlow(t)
	for _, path := range []string{"/fast", "/report?" + url.Values{"url": {upstream.URL}}.Encode()} {
		resp, err := http.Get("http://" + addr + path)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	line := slowLine(t, logs)
	if strings.Count(logs.String(), `msg="slow request"`) != 1 || strings.Contains(line, "/fast") {
		t.Errorf("want exactly one slow request, for /report:\n%s", logs.String())
	}
	field := func(name string) string {
		m := regexp.MustCompile(` ` + name + `=("[^"]*"|\S+)`).FindStringSubmatch(line)
		if m == nil {
			t.Errorf("no %s in %s", name, line)
			return ""
		}
		return strings.Trim(m[1], `"`)
	}
	number := func(s string) int {
		n, _ := strconv.Atoi(strings.TrimSuffix(s, "ms"))
		return n
	}
	if r := field("route"); r != "GET /report" {
		t.Errorf("route = %q", r)
	}
	// 50 ms upstream + 100 ms of parallel sleeps + 100 ms + 30 ms of PHP.
	if ms := number(field("ms")); ms < 280 || ms > 600 {
		t.Errorf("ms = %d", ms)
	}
	if php := number(field("php_ms")); php < 25 || php > 80 {
		t.Errorf("php_ms = %d, want about 30", php)
	}
	if run := number(field("longest_run_ms")); run < 25 || run > 80 {
		t.Errorf("longest_run_ms = %d, want about 30 (the CPU loop)", run)
	}
	if h := field("http"); !strings.HasPrefix(h, "1 in ") || number(strings.TrimPrefix(h, "1 in ")) < 45 {
		t.Errorf("http = %q, want one call of about 50ms", h)
	}
	// Four sleeps of 100 ms; three ran side by side, so they add up to more
	// than the request.
	if s := field("sleep"); !strings.HasPrefix(s, "4 in ") || number(strings.TrimPrefix(s, "4 in ")) < 390 {
		t.Errorf("sleep = %q, want 4 in about 400ms", s)
	}
}

// A request that ends without PHP's answer is logged too, with how it ended:
// here the client gives up while /report still waits.
func TestSlowRequestsEnded(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		fmt.Fprint(w, "ok")
	}))
	defer upstream.Close()

	addr, logs := startSlow(t)
	client := &http.Client{Timeout: 200 * time.Millisecond}
	resp, err := client.Get("http://" + addr + "/report?" + url.Values{"url": {upstream.URL}}.Encode())
	if err == nil {
		resp.Body.Close()
		t.Fatal("/report answered within 200ms; it should take about 280ms")
	}
	line := slowLine(t, logs)
	if !strings.Contains(line, `route="GET /report"`) || !strings.Contains(line, " ended=canceled") {
		t.Errorf("want GET /report with ended=canceled: %s", line)
	}
}

// startSlow serves examples/slow on one request thread and returns its
// address and what it logs.
func startSlow(t *testing.T) (string, *syncBuffer) {
	t.Helper()
	addr := freeAddr()
	cmd := exec.Command(cairoBin, "serve", "../examples/slow/server.php", "--threads", "1", "--offload-threads", "1")
	cmd.Env = append(os.Environ(), "CAIRO_HTTP="+addr)
	logs := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = os.Stderr, logs
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
	return addr, logs
}

// slowLine waits for a line of the slow request log and returns the last one.
func slowLine(t *testing.T, logs *syncBuffer) string {
	t.Helper()
	var line string
	deadline := time.Now().Add(2 * time.Second)
	for line == "" && time.Now().Before(deadline) {
		for _, l := range strings.Split(logs.String(), "\n") {
			if strings.Contains(l, `msg="slow request"`) {
				line = l
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if line == "" {
		t.Fatalf("no slow request logged:\n%s", logs.String())
	}
	t.Log(line)
	return line
}
