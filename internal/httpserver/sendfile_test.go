package httpserver

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// An answer names a file for Go to send only inside a declared directory:
// by its path, by the path its symlinked directory was declared with, or by
// a URL under the directory's prefix. Nothing else is found, whatever the
// name does to get out.
func TestSentFilesStayInTheirDirectory(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir, link := filepath.Join(tmp, "files"), filepath.Join(tmp, "link")
	for name, content := range map[string]string{"files/in.txt": "inside", "files/sub/deep file.txt": "deep", "secret.txt": "secret"} {
		path := filepath.Join(tmp, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(tmp, "secret.txt"), filepath.Join(dir, "out.txt")); err != nil {
		t.Fatal(err)
	}
	files, err := openSentFiles([]SentDir{{Dir: link, Prefix: "protected"}}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Sendfile-Type") != "X-Sendfile" || r.Header.Get("X-Accel-Mapping") != "" {
			t.Errorf("the request reached PHP with the client's word on X-Sendfile: %v", r.Header)
		}
		w.Header().Set(r.URL.Query().Get("header"), r.URL.Query().Get("name"))
		w.Header().Set("Content-Length", "4")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("body"))
	})
	for _, tc := range []struct {
		header, name string
		status       int
		body         string
	}{
		{"X-Sendfile", filepath.Join(dir, "in.txt"), 200, "inside"},
		{"X-Sendfile", filepath.Join(link, "in.txt"), 200, "inside"},
		{"X-Sendfile", filepath.Join(dir, "sub", "deep file.txt"), 200, "deep"},
		{"X-Accel-Redirect", "/protected/in.txt", 200, "inside"},
		{"X-Accel-Redirect", "/protected/sub/deep%20file.txt", 200, "deep"},
		{"X-Other", "x", 202, "body"},
		{"X-Sendfile", filepath.Join(tmp, "secret.txt"), 404, "not found\n"},
		{"X-Sendfile", dir + "/../secret.txt", 404, "not found\n"},
		{"X-Sendfile", dir + "x/in.txt", 404, "not found\n"},
		{"X-Sendfile", filepath.Join(dir, "out.txt"), 404, "not found\n"}, // a symlink out of the directory
		{"X-Sendfile", "in.txt", 404, "not found\n"},
		{"X-Sendfile", dir, 404, "not found\n"},
		{"X-Sendfile", filepath.Join(dir, "sub"), 404, "not found\n"},
		{"X-Accel-Redirect", "/protected/../secret.txt", 404, "not found\n"},
		{"X-Accel-Redirect", "/protected/%2e%2e/secret.txt", 404, "not found\n"},
		{"X-Accel-Redirect", "/other/in.txt", 404, "not found\n"},
		{"X-Accel-Redirect", filepath.Join(dir, "in.txt"), 404, "not found\n"},
	} {
		req := httptest.NewRequest("GET", "/?"+url.Values{"header": {tc.header}, "name": {tc.name}}.Encode(), nil)
		req.Header.Set("X-Sendfile-Type", "X-Accel-Redirect")
		req.Header.Set("X-Accel-Mapping", "/=/protected/")
		w := httptest.NewRecorder()
		files.handler(next).ServeHTTP(w, req)
		if w.Code != tc.status || w.Body.String() != tc.body {
			t.Errorf("%s: %s: %d %q, want %d %q", tc.header, tc.name, w.Code, w.Body, tc.status, tc.body)
		}
		if h := w.Header(); h.Get("X-Sendfile") != "" || h.Get("X-Accel-Redirect") != "" {
			t.Errorf("%s: %s: the client was told the name: %v", tc.header, tc.name, h)
		}
	}
}
