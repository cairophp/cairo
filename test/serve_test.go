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

// A file that serves its App itself (App::serve(), as a framework's runner
// does) and then ends with exit(0): its threads are recycled cleanly, and a
// recycled thread serves again.
func TestAppServesItselfAndEndsWithExit(t *testing.T) {
	addr := freeAddr()
	logs := &syncBuffer{}
	cmd := exec.Command(cairoBin, "serve", "../examples/serve/index.php", "--threads", "1", "--offload-threads", "1")
	cmd.Env = append(os.Environ(), "CAIRO_HTTP="+addr, "MAX_REQUESTS=3")
	cmd.Stdout, cmd.Stderr = os.Stderr, io.MultiWriter(os.Stderr, logs)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		_ = cmd.Wait()
	})
	if err := waitFor(addr, 20*time.Second); err != nil {
		t.Fatal(err)
	}
	for i := range 12 {
		want := fmt.Sprintf("Hello r%d\n", i)
		if code, body := getBody(t, fmt.Sprintf("http://%s/hello?name=r%d", addr, i)); code != 200 || body != want {
			t.Fatalf("request %d: %d %q", i, code, body)
		}
	}
	waitUntil(t, "a thread is recycled", func() bool { return strings.Contains(logs.String(), "php thread recycled") })
	if strings.Contains(logs.String(), "stopped unexpectedly") {
		t.Fatalf("a recycled thread was reported as a crash:\n%s", logs.String())
	}
}

func getBody(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}
