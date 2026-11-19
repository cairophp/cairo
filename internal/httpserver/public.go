package httpserver

import (
	"fmt"
	"net/http"
	"os"
	"path"
	"strings"
)

// publicFiles serves the regular files under dir at their own paths, as a
// web server's document root does, and hands every other request to next:
// Laravel's public/ next to its routes. os.Root keeps every lookup inside
// dir, symlinks included.
func publicFiles(dir string, next http.Handler) (http.Handler, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("public dir: %w", err)
	}
	files := http.FileServerFS(root.FS())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			if name, ok := publicName(r.URL.Path); ok {
				if fi, err := root.Stat(name); err == nil && fi.Mode().IsRegular() {
					files.ServeHTTP(w, r)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	}), nil
}

// publicName is the file a URL path names under the root, or false for paths
// never served as files: the root itself, PHP sources (index.php is the app,
// not a download) and hidden files such as .env or .git/config.
func publicName(urlPath string) (string, bool) {
	name := strings.TrimPrefix(path.Clean("/"+urlPath), "/")
	if name == "" || strings.EqualFold(path.Ext(name), ".php") {
		return "", false
	}
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") {
			return "", false
		}
	}
	return name, true
}
