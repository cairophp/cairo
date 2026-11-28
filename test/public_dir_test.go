package test

import (
	"io"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"
)

// $http->publicDir(): files that exist are served by Go, everything else
// (missing files, PHP sources, hidden files but a top-level .well-known)
// reaches the app.
func TestPublicDir(t *testing.T) {
	addr := freeAddr()
	cmd := exec.Command(cairoBin, "serve", "../examples/public/server.php", "--threads", "1", "--offload-threads", "1")
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
	for _, c := range []struct{ path, want string }{
		{"/robots.txt", "User-agent: *\n"},
		{"/css/app.css", "body{}\n"},
		{"/", "app /"},
		{"/users/7", "app /users/7"},
		{"/css/missing.css", "app /css/missing.css"},
		{"/index.php", "app /index.php"},
		{"/.env", "app /.env"},
		{"/.well-known/security.txt", "Contact: https://github.com/cairophp/cairo/security\n"},
		{"/css/.well-known/x", "app /css/.well-known/x"},
	} {
		resp, err := http.Get("http://" + addr + c.path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(b) != c.want {
			t.Errorf("GET %s: got %q, want %q", c.path, b, c.want)
		}
	}
}
