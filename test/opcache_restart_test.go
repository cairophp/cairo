package test

import (
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// startOpcache serves a copy of testdata/opcache, with its php.ini, on two
// PHP threads that are recycled every 10 requests. get is the status and
// the body of GET path, or 0 when the server did not answer.
func startOpcache(t *testing.T) (dir string, logs *syncBuffer, get func(path string) (int, string)) {
	t.Helper()
	if os.Getenv("CAIRO_TEST_NO_OPCACHE") != "" {
		t.Skip("this binary has no opcache")
	}
	if !opcacheTellsOfRestart(t) {
		t.Skip("PHP older than 8.4: opcache does not tell of its restart, and the server still dies of it")
	}
	dir = t.TempDir()
	code, err := os.ReadFile("testdata/opcache/server.php")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "server.php"), code, 0o644); err != nil {
		t.Fatal(err)
	}
	bigFile(t, dir, 0)
	ini, _ := filepath.Abs("testdata/opcache/php.ini")
	addr := freeAddr()
	cmd := exec.Command(cairoBin, "serve", filepath.Join(dir, "server.php"), "--threads", "2", "--offload-threads", "1", "--php-ini", ini)
	cmd.Env = append(os.Environ(), "CAIRO_HTTP="+addr)
	logs = &syncBuffer{}
	cmd.Stdout, cmd.Stderr = os.Stderr, io.MultiWriter(os.Stderr, logs)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	t.Cleanup(func() {
		client.CloseIdleConnections()
		_ = cmd.Process.Signal(os.Interrupt)
		_ = cmd.Wait()
	})
	if err := waitFor(addr, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	return dir, logs, func(path string) (int, string) {
		resp, err := client.Get("http://" + addr + path)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
}

// opcacheTellsOfRestart is whether the binary's PHP has the hook that the
// start gate needs (zend_accel_schedule_restart_hook, PHP 8.4).
func opcacheTellsOfRestart(t *testing.T) bool {
	t.Helper()
	return phpVersionID(t) >= 80400
}

// phpVersionID is the binary's PHP_VERSION_ID.
func phpVersionID(t *testing.T) int {
	t.Helper()
	out, err := exec.Command(cairoBin, "php", "-r", "echo PHP_VERSION_ID;").Output()
	if err != nil {
		t.Fatalf("cairo php: %v", err)
	}
	id, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("PHP_VERSION_ID: %q", out)
	}
	return id
}

// bigFile writes version v of big.php into dir: half a megabyte of
// functions that return the version. Its time is v seconds after a fixed
// one, because opcache tells versions apart by the second.
func bigFile(t *testing.T, dir string, v int) {
	t.Helper()
	var code strings.Builder
	code.WriteString("<?php\n")
	version := "v" + strconv.Itoa(v)
	for i := 0; i < 4000; i++ {
		n := strconv.Itoa(i)
		code.WriteString("function big_" + n + "(): string { static $t = ['" + strings.Repeat(version+"x", 16) + "', " + n + ", '" + version + "']; return $t[2]; }\n")
	}
	next := filepath.Join(dir, "big.next")
	if err := os.WriteFile(next, []byte(code.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1_700_000_000+int64(v), 0)
	if err := os.Chtimes(next, at, at); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, filepath.Join(dir, "big.php")); err != nil {
		t.Fatal(err)
	}
}

// restartedInPlace waits for the server to start anew and to answer again,
// and checks that no thread was lost on the way.
func restartedInPlace(t *testing.T, logs *syncBuffer, get func(string) (int, string), reason string) {
	t.Helper()
	waitUntil(t, "the server restarts in place", func() bool {
		get("/") // a recycled thread is what finds that it cannot start
		return strings.Count(logs.String(), "http server listening") >= 2
	})
	waitUntil(t, "the new process answers", func() bool { code, _ := get("/"); return code == 200 })
	log := logs.String()
	if !strings.Contains(log, "restarting in place") || !strings.Contains(log, reason) {
		t.Errorf("the log does not say that the server restarts in place because %q:\n%s", reason, log)
	}
	for _, bad := range []string{"corrupted", "stopped unexpectedly", "SIGSEGV", "SIGABRT"} {
		if strings.Contains(log, bad) {
			t.Errorf("the log has %q:\n%s", bad, log)
		}
	}
}

// opcache wipes its cache at the start of the next PHP request once it is
// full of stale code, without seeing the other threads of the process that
// run code from it: the server died of that ("zend_mm_heap corrupted").
// Now the thread that would start is not let in, and the server starts anew
// in place, with the newest code.
func TestFullOpcacheRestartsTheServerInPlace(t *testing.T) {
	dir, logs, get := startOpcache(t)
	if code, body := get("/"); code != 200 || body != "v0" {
		t.Fatalf("GET /: %d %q, want v0", code, body)
	}
	last := 0
	for v := 1; v <= 12 && !strings.Contains(logs.String(), "restarting in place"); v++ {
		bigFile(t, dir, v)
		last = v
		// The threads are recycled on the way, and the new ones read version v.
		for i := 0; i < 60; i++ {
			get("/")
		}
	}
	restartedInPlace(t, logs, get, "opcache's memory is full")
	if _, body := get("/"); body != "v"+strconv.Itoa(last) {
		t.Errorf("after the restart: %q, want v%d", body, last)
	}
}

// opcache_reset() asks for the same restart.
func TestOpcacheResetRestartsTheServerInPlace(t *testing.T) {
	_, logs, get := startOpcache(t)
	if code, body := get("/reset"); code != 200 || body != "reset" {
		t.Fatalf("GET /reset: %d %q", code, body)
	}
	restartedInPlace(t, logs, get, "opcache_reset() was called")
}
