package test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer collects the server's stderr while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// The cairo SAPI: PHP_SAPI is "cairo", a php.ini is read (but cannot turn on
// PHP's time limits), dl() is not offered, and PHP's log lines reach Cairo's
// logger once each.
func TestCairoSAPI(t *testing.T) {
	ini, _ := filepath.Abs("testdata/sapi/php.ini")
	addr := freeAddr()
	cmd := exec.Command(cairoBin, "serve", "testdata/sapi/server.php", "--threads", "1", "--offload-threads", "1", "--php-ini", ini)
	cmd.Env = append(os.Environ(), "CAIRO_HTTP="+addr)
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
	httpGet := func(path string) string {
		saved := httpAddr
		httpAddr = addr
		defer func() { httpAddr = saved }()
		return get(t, path)
	}

	var out map[string]any
	raw := httpGet("/sapi")
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	want := map[string]any{
		"sapi": "cairo", "php_sapi_name": "cairo", "ini_file": ini, "memory_limit": "77M",
		"max_execution_time": "0", "display_errors": "0", "dl": false,
	}
	for k, v := range want {
		if out[k] != v {
			t.Errorf("%s = %#v, want %#v (%s)", k, out[k], v, raw)
		}
	}

	httpGet("/log?msg=hi")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(stderr.String(), "a warning from php") {
		time.Sleep(20 * time.Millisecond)
	}
	logs := stderr.String()
	for _, line := range []string{`msg="hello from php: hi" source=php`, `a warning from php`} {
		if n := strings.Count(logs, line); n != 1 {
			t.Errorf("%q logged %d times, want once:\n%s", line, n, logs)
		}
	}
	if !strings.Contains(logs, "level=WARN msg=\"PHP Warning:  a warning from php") {
		t.Errorf("the warning was not logged at WARN:\n%s", logs)
	}
}
