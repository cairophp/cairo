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

// examples/leaks, in both modes: the unregistered Tenant::$current and the
// growing list Tenant::$visitors are logged once each, however often they
// change; the registered Tenant::$local, the cache Tenant::$config and
// Cairo's own statics are not.
func TestDetectLeaks(t *testing.T) {
	for _, mode := range []string{"fibers", "isolated"} {
		t.Run(mode, func(t *testing.T) {
			addr := freeAddr()
			cmd := exec.Command(cairoBin, "serve", "../examples/leaks/server.php", "--threads", "1")
			cmd.Env = append(os.Environ(), "CAIRO_HTTP="+addr)
			if mode == "isolated" {
				cmd.Env = append(cmd.Env, "CAIRO_ISOLATED=1")
			}
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
			for _, path := range []string{
				"/config?key=a", "/config?key=a", "/config?key=b", "/local?name=a", "/local?name=b",
				"/tenant?name=a", "/tenant?name=b", "/tenant?name=c", "/tenant?name=d", "/visit?name=a",
				"/visit?name=b", "/visit?name=c", "/visit?name=d", "/config?key=c",
			} {
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
			for deadline := time.Now().Add(2 * time.Second); len(lines) < 2 && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
				lines = lines[:0]
				for _, l := range strings.Split(logs.String(), "\n") {
					if strings.Contains(l, `msg="static changed by a request"`) {
						lines = append(lines, l)
					}
				}
			}
			all := strings.Join(lines, "\n")
			for _, want := range []string{`route="GET /tenant" static=Tenant::$current`, `route="GET /visit" static=Tenant::$visitors`} {
				if len(lines) != 2 || !strings.Contains(all, want) {
					t.Fatalf("want 2 lines, one with %s:\n%s", want, logs.String())
				}
			}
		})
	}
}
