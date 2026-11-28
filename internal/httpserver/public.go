package httpserver

import (
	"fmt"
	"net/http"
	"os"
	"path"
	"strings"
)

// confined serves files from one directory through an os.Root, which keeps
// every lookup inside it, symlinks included, and only the files publicName
// allows: no PHP source, no hidden file, no directory listing.
type confined struct {
	root  *os.Root
	files http.Handler
}

func openConfined(dir string) (*confined, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &confined{root: root, files: http.FileServerFS(root.FS())}, nil
}

// serve answers r with the file its URL path names and reports true, or
// reports false and leaves w alone when that is no file to serve. With
// index, a directory is served by its index.html, as a web server does.
func (c *confined) serve(w http.ResponseWriter, r *http.Request, index bool) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	name, ok := publicName(r.URL.Path)
	if !ok {
		return false
	}
	fi, err := c.root.Stat(name)
	if err == nil && fi.IsDir() && index {
		fi, err = c.root.Stat(path.Join(name, "index.html"))
	}
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	c.files.ServeHTTP(w, r)
	return true
}

// publicFiles serves the regular files under dir at their own paths, as a
// web server's document root does, and hands every other request to next:
// Laravel's public/ next to its routes.
func publicFiles(dir string, next http.Handler) (http.Handler, error) {
	files, err := openConfined(dir)
	if err != nil {
		return nil, fmt.Errorf("public dir: %w", err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !files.serve(w, r, false) {
			next.ServeHTTP(w, r)
		}
	}), nil
}

// staticFiles is $http->static(): the files under dir at prefix, by the
// rules of publicDir(), and 404 for everything else under prefix.
func staticFiles(prefix, dir string) (http.Handler, error) {
	files, err := openConfined(dir)
	if err != nil {
		return nil, fmt.Errorf("static %s: %w", prefix, err)
	}
	return http.StripPrefix(prefix, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !files.serve(w, r, true) {
			http.NotFound(w, r)
		}
	})), nil
}

// publicName is the file a URL path names under the root, or false for paths
// never served as files: the root itself, PHP sources (index.php is the app,
// not a download) and hidden files such as .env or .git/config. The one
// hidden directory served is a top-level .well-known (RFC 8615: security.txt,
// ACME challenges, app links).
func publicName(urlPath string) (string, bool) {
	name := strings.TrimPrefix(path.Clean("/"+urlPath), "/")
	if name == "" || strings.EqualFold(path.Ext(name), ".php") {
		return "", false
	}
	for i, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") && (i > 0 || part != ".well-known") {
			return "", false
		}
	}
	return name, true
}
