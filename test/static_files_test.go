package test

import (
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// $http->static() serves files by publicDir()'s rules: no PHP source, no
// hidden file, no directory listing and nothing outside its directory, a
// symlink's target included; a symlink that stays inside is served.
func TestStaticServesOnlyItsFiles(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	write := func(name, text string) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("ok.txt", "ok\n")
	write(".env", "SECRET=1\n")
	write("config.php", "<?php $password = 1;\n")
	write("site/index.html", "index\n")
	write("listed/a.txt", "a\n")
	write(".well-known/security.txt", "Contact: x\n")
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{
		"out.txt": filepath.Join(outside, "secret.txt"),
		"outdir":  outside,
		"in.txt":  "ok.txt",
	} {
		if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}

	addr := freeAddr()
	cmd := exec.Command(cairoBin, "serve", "../examples/public/server.php", "--threads", "1", "--offload-threads", "1")
	cmd.Env = append(os.Environ(), "CAIRO_HTTP="+addr, "CAIRO_STATIC_DIR="+dir)
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
	for _, c := range []struct {
		path   string
		status int
		want   string
	}{
		{"/assets/ok.txt", 200, "ok\n"},
		{"/assets/in.txt", 200, "ok\n"},
		{"/assets/site/", 200, "index\n"},
		{"/assets/.well-known/security.txt", 200, "Contact: x\n"},
		{"/assets/.env", 404, ""},
		{"/assets/config.php", 404, ""},
		{"/assets/listed/", 404, ""},
		{"/assets/", 404, ""},
		{"/assets/out.txt", 404, ""},
		{"/assets/outdir/secret.txt", 404, ""},
		{"/assets/missing.txt", 404, ""},
	} {
		resp, err := http.Get("http://" + addr + c.path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != c.status || (c.status == 200 && string(b) != c.want) {
			t.Errorf("GET %s: %d %q, want %d %q", c.path, resp.StatusCode, b, c.status, c.want)
		}
	}
}
