package httpserver

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// A file that PHP names instead of sending it ($http->sendfile()): the
// answer has no body but the header X-Sendfile with the file's path, or
// X-Accel-Redirect with a URL path, as code written for Apache or nginx
// says it. Go sends the file, with ranges and If-Modified-Since, and the
// PHP thread is free meanwhile.

// SentDir is one $http->sendfile(): a directory that answers may name
// files of, and the URL prefix that stands for it in X-Accel-Redirect.
type SentDir struct {
	Dir    string `json:"dir"`
	Prefix string `json:"prefix"`
}

// sentDir is a SentDir that is open: os.Root keeps every lookup inside it,
// symlinks included.
type sentDir struct {
	root   *os.Root
	paths  []string // the directory as declared and with its symlinks resolved, each ending in a separator
	prefix string   // "/files/", or "" for none
}

type sentFiles struct {
	dirs []sentDir
	log  *slog.Logger
}

func openSentFiles(decl []SentDir, log *slog.Logger) (*sentFiles, error) {
	s := &sentFiles{log: log}
	for _, d := range decl {
		abs, err := filepath.Abs(d.Dir)
		if err != nil {
			return nil, fmt.Errorf("sendfile: %w", err)
		}
		// A framework names a file by its real path.
		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return nil, fmt.Errorf("sendfile: %w", err)
		}
		root, err := os.OpenRoot(real)
		if err != nil {
			return nil, fmt.Errorf("sendfile: %w", err)
		}
		dir := sentDir{root: root, paths: []string{real + string(filepath.Separator)}}
		if abs != real {
			dir.paths = append(dir.paths, abs+string(filepath.Separator))
		}
		if p := strings.Trim(d.Prefix, "/"); p != "" {
			dir.prefix = "/" + p + "/"
		}
		s.dirs = append(s.dirs, dir)
	}
	return s, nil
}

// handler serves next, and the files its answers name. What a client says
// of X-Sendfile is replaced: a framework that trusts those headers (Symfony's
// BinaryFileResponse::trustXSendfileTypeHeader()) then leaves its files to Go.
func (s *sentFiles) handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		req.Header.Set("X-Sendfile-Type", "X-Sendfile")
		req.Header.Del("X-Accel-Mapping")
		next.ServeHTTP(&sendfileWriter{ResponseWriter: w, req: req, files: s}, req)
	})
}

// sendfileWriter looks at the head of an answer: one that names a file is
// answered with the file, and what PHP writes as its body is dropped.
type sendfileWriter struct {
	http.ResponseWriter
	req   *http.Request
	files *sentFiles
	began bool
	file  bool
}

// Unwrap is for http.ResponseController.
func (w *sendfileWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *sendfileWriter) WriteHeader(status int) {
	if w.began || status < 200 { // an informational response is not the answer
		if !w.file {
			w.ResponseWriter.WriteHeader(status)
		}
		return
	}
	w.began = true
	h := w.Header()
	name, accel := h.Get("X-Sendfile"), false
	if name == "" {
		name, accel = h.Get("X-Accel-Redirect"), true
	}
	if name == "" {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.file = true
	h.Del("X-Sendfile")
	h.Del("X-Accel-Redirect")
	// The file's, which ServeContent sets: PHP's are those of no body.
	h.Del("Content-Length")
	h.Del("Content-Range")
	w.files.serve(w.ResponseWriter, w.req, name, accel)
}

func (w *sendfileWriter) Write(b []byte) (int, error) {
	if !w.began {
		w.WriteHeader(http.StatusOK)
	}
	if w.file {
		return len(b), nil
	}
	return w.ResponseWriter.Write(b)
}

// serve sends the file that an answer named: a path of the file system
// (X-Sendfile) or, with accel, a URL path under a directory's prefix. A
// name outside the declared directories is not found, and the log says why.
func (s *sentFiles) serve(w http.ResponseWriter, req *http.Request, name string, accel bool) {
	root, rel := s.find(name, accel)
	if root == nil {
		s.log.Warn("an answer named a file outside the directories of sendfile()", "file", name)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	f, err := root.Open(rel)
	if err != nil {
		s.log.Warn("an answer named a file that cannot be sent", "file", name, "err", err)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		s.log.Warn("an answer named what is not a regular file", "file", name)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	http.ServeContent(w, req, fi.Name(), fi.ModTime(), f)
}

// find returns the directory that name is in and its path there, or nil.
func (s *sentFiles) find(name string, accel bool) (*os.Root, string) {
	if accel {
		// nginx reads the value as a URL.
		if decoded, err := url.PathUnescape(name); err == nil {
			name = decoded
		}
		name = path.Clean("/" + name)
		for _, d := range s.dirs {
			if d.prefix != "" && strings.HasPrefix(name, d.prefix) {
				return d.root, filepath.FromSlash(name[len(d.prefix):])
			}
		}
		return nil, ""
	}
	if !filepath.IsAbs(name) {
		return nil, ""
	}
	name = filepath.Clean(name)
	for _, d := range s.dirs {
		for _, p := range d.paths {
			if strings.HasPrefix(name, p) {
				return d.root, name[len(p):]
			}
		}
	}
	return nil, ""
}
