package httpserver

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// scripts maps a request to a PHP script under a directory, as a web server
// in front of PHP-FPM does (nginx's try_files with fastcgi_split_path_info):
// $http->scripts(), for apps that are a set of scripts. Go serves the other
// files of the directory (publicFiles); this only names the script, and
// PHP runs it in a PHP request of its own (classic mode).
type scripts struct {
	root *os.Root
	dir  string // absolute, the directory PHP opens the scripts in
}

// script is what PHP gets of the script that answers a request.
type script struct {
	file     string // the absolute path PHP runs
	name     string // its URL path, SCRIPT_NAME
	pathInfo string // what follows it in the request's path, PATH_INFO
	port     string // the port the request came in on, SERVER_PORT
	dir      string // DOCUMENT_ROOT
}

type scriptKey struct{}

func openScripts(dir string) (*scripts, error) {
	abs, err := filepath.Abs(dir)
	if err == nil {
		abs, err = filepath.EvalSymlinks(abs)
	}
	if err != nil {
		return nil, fmt.Errorf("scripts: %w", err)
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, fmt.Errorf("scripts: %w", err)
	}
	return &scripts{root: root, dir: abs}, nil
}

// handler passes on, with its script, each request that has one, and
// answers the others itself: 404, or a redirect from a directory to its
// path with a slash, whose index.php then answers.
func (s *scripts) handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		sc, redirect := s.find(req.URL.Path)
		switch {
		case redirect:
			u := *req.URL
			u.Path += "/"
			http.Redirect(w, req, u.String(), http.StatusMovedPermanently)
		case sc == nil:
			http.NotFound(w, req)
		default:
			if addr, ok := req.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
				_, sc.port, _ = net.SplitHostPort(addr.String())
			}
			next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), scriptKey{}, sc)))
		}
	})
}

// find names the script of a URL path: the first part of the path that is
// a .php file, with the rest as its PATH_INFO; else the index.php of the
// directory the path names; else the index.php of the root, the front
// controller (WordPress' pretty links). A directory named without its
// slash is redirected to it. Hidden parts (.git, .env) are never a file
// here, and os.Root keeps every lookup inside the directory, symlinks
// included.
func (s *scripts) find(urlPath string) (sc *script, redirect bool) {
	clean := path.Clean("/" + urlPath)
	parts := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	for i, part := range parts {
		if strings.HasPrefix(part, ".") {
			return s.front(), false
		}
		if strings.EqualFold(path.Ext(part), ".php") {
			name := strings.Join(parts[:i+1], "/")
			if s.isFile(name) {
				return s.at(name, "/"+strings.Join(parts[i+1:], "/")), false
			}
		}
	}
	name := strings.TrimPrefix(clean, "/")
	if fi, err := s.root.Stat(dirName(name)); err == nil && fi.IsDir() && s.isFile(path.Join(name, "index.php")) {
		if name != "" && !strings.HasSuffix(urlPath, "/") {
			return nil, true
		}
		return s.at(path.Join(name, "index.php"), ""), false
	}
	return s.front(), false
}

// dirName is how os.Root names the directory of a cleaned URL path.
func dirName(name string) string {
	if name == "" {
		return "."
	}
	return name
}

func (s *scripts) front() *script {
	if !s.isFile("index.php") {
		return nil
	}
	return s.at("index.php", "")
}

func (s *scripts) isFile(name string) bool {
	fi, err := s.root.Stat(name)
	return err == nil && fi.Mode().IsRegular()
}

func (s *scripts) at(name, pathInfo string) *script {
	if pathInfo == "/" {
		pathInfo = ""
	}
	return &script{file: filepath.Join(s.dir, filepath.FromSlash(name)), name: "/" + name, pathInfo: pathInfo, dir: s.dir}
}
