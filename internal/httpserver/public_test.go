package httpserver

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestPublicFiles(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("robots.txt", "robots")
	write("build/app.css", "css")
	write("index.php", "<?php source")
	write("upload.PHP", "<?php source")
	write(".env", "SECRET=1")
	write(".well-known/x", "well-known")
	write("css/.well-known/x", "hidden dir")
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}

	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "app "+r.URL.Path)
	})
	h, err := publicFiles(dir, app)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ method, path, want string }{
		{"GET", "/robots.txt", "robots"},
		{"GET", "/build/app.css", "css"},
		{"HEAD", "/robots.txt", ""},
		{"POST", "/robots.txt", "app /robots.txt"},
		{"GET", "/", "app /"},
		{"GET", "/build", "app /build"},
		{"GET", "/missing.css", "app /missing.css"},
		{"GET", "/index.php", "app /index.php"},
		{"GET", "/upload.PHP", "app /upload.PHP"},
		{"GET", "/.env", "app /.env"},
		{"GET", "/.well-known/x", "well-known"},
		{"GET", "/css/.well-known/x", "app /css/.well-known/x"},
		{"GET", "/link", "app /link"},
		{"GET", "/../" + filepath.Base(outside) + "/secret", "app /../" + filepath.Base(outside) + "/secret"},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(c.method, "http://x"+c.path, nil)
		h.ServeHTTP(rec, req)
		if got := rec.Body.String(); got != c.want {
			t.Errorf("%s %s: got %q, want %q", c.method, c.path, got, c.want)
		}
	}
}
