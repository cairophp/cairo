package test

import (
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// With the repeated query detector and the leak detector both on, a request
// that each has something to say about is reported by both: its repeated
// query first, then the static it changed.
func TestDetectorsTogether(t *testing.T) {
	addr := freeAddr()
	cmd := exec.Command(cairoBin, "serve", "testdata/detectors/server.php", "--threads", "1")
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
	// The first request sets the static, which a setting does once too. The
	// second changes it again and runs one statement three times.
	for _, path := range []string{"/both?name=a", "/both?name=b&queries=1"} {
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
	const (
		query  = `msg="repeated query" route="GET /both" count=3 query="SELECT ? AS id"`
		static = `msg="static changed by a request" route="GET /both" static=Seen::$last`
	)
	q, s := -1, -1
	for deadline := time.Now().Add(2 * time.Second); (q < 0 || s < 0) && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		all := logs.String()
		q, s = strings.Index(all, query), strings.Index(all, static)
	}
	if q < 0 || s < 0 || q > s {
		t.Fatalf("want the repeated query, then the static:\n%s", logs.String())
	}
}
