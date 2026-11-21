package test

import (
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// startSuperviseLogged runs examples/supervise on one PHP thread. stop ends
// the server as a deploy does and returns everything it logged.
func startSuperviseLogged(t *testing.T) (addr string, stop func() string) {
	t.Helper()
	addr = freeAddr()
	cmd := exec.Command(cairoBin, "serve", "../examples/supervise/server.php", "--threads", "1", "--offload-threads", "1")
	cmd.Env = append(os.Environ(), "CAIRO_HTTP="+addr)
	logs := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = os.Stderr, logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill() // no-op once stop has run
		_ = cmd.Wait()
	})
	if err := waitFor(addr, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	return addr, func() string {
		_ = cmd.Process.Signal(os.Interrupt)
		if err := cmd.Wait(); err != nil {
			t.Errorf("cairo serve after the interrupt: %v", err)
		}
		return logs.String()
	}
}

// A request whose client left still has cleanup to do when the server is
// told to stop: its thread finishes it before it ends, instead of leaving
// the request's fiber for PHP to destroy halfway.
func TestStopFinishesRequestItsClientLeft(t *testing.T) {
	addr, stop := startSuperviseLogged(t)
	client := &http.Client{Timeout: 200 * time.Millisecond}
	if resp, err := client.Get("http://" + addr + "/left"); err == nil {
		resp.Body.Close()
		t.Fatal("/left answered; want the client to give up first")
	}
	logs := stop()
	if !strings.Contains(logs, "left: cleaned up") {
		t.Errorf("the request was cut off at the stop, want its cleanup to finish:\n%s", logs)
	}
	if strings.Contains(logs, "FiberError") {
		t.Errorf("a fiber error at the stop:\n%s", logs)
	}
}

// exit() ends a thread while a task of the same request waits. PHP destroys
// the task's fiber, the task catches what its cleanup throws meanwhile and
// returns: the fiber ends there, and does not try to wait for a next job.
func TestExitEndsWaitingFibersQuietly(t *testing.T) {
	addr, stop := startSuperviseLogged(t)
	client := &http.Client{Timeout: 10 * time.Second}
	for _, want := range []struct {
		path string
		code int
	}{{"/exit", 500}, {"/boot", 200}} {
		resp, err := client.Get("http://" + addr + want.path)
		if err != nil {
			t.Fatalf("GET %s: %v", want.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != want.code {
			t.Fatalf("%s: status %d, want %d", want.path, resp.StatusCode, want.code)
		}
	}
	if logs := stop(); strings.Contains(logs, "FiberError") || !strings.Contains(logs, "php thread restarted") {
		t.Errorf("want a restarted thread and no fiber error:\n%s", logs)
	}
}
