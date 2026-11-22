// Package httpserver serves the HTTP servers declared in server.php. Go does
// the listening, routing, timeouts and static files; only matched routes reach
// PHP.
package httpserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/cairophp/cairo/internal/middleware"
	"github.com/cairophp/cairo/internal/wire"
)

// Dispatcher sends a call to PHP and returns PHP's answer: its metadata
// (JSON) and the response body.
type Dispatcher interface {
	Dispatch(ctx context.Context, ev wire.Event) (meta, body []byte, err error)
}

// maxBody caps request bodies read into memory for PHP.
const maxBody = 32 << 20

var paramRe = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_]*)(\.\.\.)?\}`)

// Server is one HTTP listener.
type Server struct {
	srv *http.Server
	ln  net.Listener
	log *slog.Logger
}

// New builds the router for one declared HTTP server and opens its listener.
func New(decl Declaration, php Dispatcher, log *slog.Logger) (*Server, error) {
	mux := http.NewServeMux()
	cache := newResponseCache(cacheMaxBytes)
	for _, r := range decl.Routes {
		pattern := r.Method + " " + r.Path
		if r.Method == "*" {
			pattern = r.Path // $http->fallback(): any method
		}
		mux.Handle(pattern, routeHandler(r, php, cache, log))
	}
	for _, s := range decl.Static {
		prefix := "/" + strings.Trim(s.Prefix, "/") + "/"
		mux.Handle("GET "+prefix, http.StripPrefix(prefix, http.FileServer(http.Dir(s.Dir))))
	}
	var routes http.Handler = mux
	if decl.PublicDir != "" {
		var err error
		if routes, err = publicFiles(decl.PublicDir, mux); err != nil {
			return nil, err
		}
	}
	handler, err := middleware.HTTP(routes, decl.Middleware, log)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", decl.Listen)
	if err != nil {
		return nil, err
	}
	return &Server{
		srv: &http.Server{
			Handler:      handler,
			ReadTimeout:  seconds(decl.ReadTimeout),
			WriteTimeout: seconds(decl.WriteTimeout),
		},
		ln:  ln,
		log: log,
	}, nil
}

// Addr is the address the server listens on.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Serve blocks until the server stops.
func (s *Server) Serve() error {
	err := s.srv.Serve(s.ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown stops accepting connections and waits for in-flight requests.
func (s *Server) Shutdown(ctx context.Context) error { return s.srv.Shutdown(ctx) }

// event is a request as PHP receives it (Cairo\Http\Request).
type event struct {
	route   int
	name    string // "GET /users/{id}", for logs
	method  string
	path    string
	query   string
	headers http.Header
	params  map[string]string
	body    []byte
	remote  string
	host    string
	proto   string
	tls     bool
	values  map[string]any
	form    *multipartForm
}

// AppendWire encodes the request for PHP; the body goes as raw bytes.
// RouteName is the route's method and pattern, for the slow request log.
func (ev *event) RouteName() string { return ev.name }

func (ev *event) AppendWire(b []byte, id int64, deadline float64) []byte {
	n := 13
	if deadline > 0 {
		n++
	}
	if len(ev.values) > 0 {
		n++
	}
	if ev.form != nil {
		n += 2
	}
	b = wire.AppendMap(b, n)
	b = wire.AppendString(wire.AppendKey(b, "type"), "http")
	b = wire.AppendInt(wire.AppendKey(b, "id"), id)
	if deadline > 0 {
		b = wire.AppendFloat(wire.AppendKey(b, "deadline"), deadline)
	}
	b = wire.AppendInt(wire.AppendKey(b, "route"), int64(ev.route))
	b = wire.AppendString(wire.AppendKey(b, "method"), ev.method)
	b = wire.AppendString(wire.AppendKey(b, "path"), ev.path)
	b = wire.AppendString(wire.AppendKey(b, "query"), ev.query)
	b = wire.AppendHeader(wire.AppendKey(b, "headers"), ev.headers)
	b = wire.AppendValue(wire.AppendKey(b, "params"), ev.params)
	b = wire.AppendBytes(wire.AppendKey(b, "body"), ev.body)
	b = wire.AppendString(wire.AppendKey(b, "remote"), ev.remote)
	b = wire.AppendString(wire.AppendKey(b, "host"), ev.host)
	b = wire.AppendString(wire.AppendKey(b, "proto"), ev.proto)
	b = wire.AppendBool(wire.AppendKey(b, "tls"), ev.tls)
	if len(ev.values) > 0 {
		b = wire.AppendValue(wire.AppendKey(b, "values"), ev.values)
	}
	if ev.form != nil {
		b = wire.AppendList(wire.AppendKey(b, "form"), len(ev.form.fields))
		for _, f := range ev.form.fields {
			b = wire.AppendString(wire.AppendString(wire.AppendList(b, 2), f[0]), f[1])
		}
		b = wire.AppendList(wire.AppendKey(b, "files"), len(ev.form.files))
		for _, f := range ev.form.files {
			b = wire.AppendMap(b, 7)
			b = wire.AppendString(wire.AppendKey(b, "field"), f.Field)
			b = wire.AppendString(wire.AppendKey(b, "name"), f.Name)
			b = wire.AppendString(wire.AppendKey(b, "full_path"), f.FullPath)
			b = wire.AppendString(wire.AppendKey(b, "type"), f.Type)
			b = wire.AppendInt(wire.AppendKey(b, "size"), f.Size)
			b = wire.AppendString(wire.AppendKey(b, "tmp_name"), f.TmpName)
			b = wire.AppendInt(wire.AppendKey(b, "error"), int64(f.Error))
		}
	}
	return b
}

func routeHandler(r Route, php Dispatcher, cache *responseCache, log *slog.Logger) http.Handler {
	var names []string
	for _, m := range paramRe.FindAllStringSubmatch(r.Path, -1) {
		names = append(names, m[1])
	}
	name := r.Method + " " + r.Path
	var policy *cachePolicy
	if r.Cache != nil {
		policy = newCachePolicy(r.ID, *r.Cache)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// The cache first: a request it answers needs no event, because PHP
		// does not see it. A multipart request never uses the cache.
		key, cached := policy.key(req)
		cached = cached && !isMultipart(req.Header)
		var stale *response // served while this request's call rebuilds it
		if cached {
			resp, refresh := cache.lookup(key, policy, req.Method == http.MethodGet)
			if resp != nil && !refresh {
				resp.write(w)
				return
			}
			stale = resp
			// PHP may answer HEAD without a body, so HEAD only reads what a
			// GET stored and never fills the cache.
			cached = req.Method == http.MethodGet
		}
		ev, cleanup, ok := newEvent(w, req, r.ID, names)
		if !ok {
			if stale != nil {
				cache.release(key)
			}
			return
		}
		ev.name = name
		defer cleanup()
		if !cached {
			resp, err := callPHP(req.Context(), php, ev)
			if err != nil {
				writeError(w, req, err, log)
				return
			}
			resp.write(w)
			return
		}
		call := func(ctx context.Context) (*response, error) {
			r, err := callPHP(ctx, php, ev)
			return &r, err
		}
		if stale != nil {
			cache.refresh(req.Context(), key, policy, call)
			stale.write(w)
			return
		}
		resp, err := cache.fetch(req.Context(), key, policy, call)
		if err != nil {
			writeError(w, req, err, log)
			return
		}
		resp.write(w)
	})
}

// isMultipart reports whether the request's body is a form with parts,
// which PHP receives as fields and files.
func isMultipart(h http.Header) bool {
	const prefix = "multipart/form-data"
	ct := h.Get("Content-Type")
	return len(ct) >= len(prefix) && strings.EqualFold(ct[:len(prefix)], prefix)
}

// newEvent reads req into the event PHP receives. It answers the client
// itself and returns false when the request is malformed.
func newEvent(w http.ResponseWriter, req *http.Request, route int, names []string) (*event, func(), bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxBody))
	if err != nil {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return nil, nil, false
	}
	var params map[string]string // nil crosses as an empty array, as a route without parameters has
	if len(names) > 0 {
		params = make(map[string]string, len(names))
		for _, n := range names {
			params[n] = req.PathValue(n)
		}
	}
	cleanup := func() {}
	var form *multipartForm
	if isMultipart(req.Header) {
		form, err = parseMultipart(body, req.Header.Get("Content-Type"), os.TempDir())
		if err != nil {
			http.Error(w, "malformed multipart body", http.StatusBadRequest)
			return nil, nil, false
		}
		cleanup = form.cleanup
		body = nil // like PHP, php://input is empty for multipart/form-data
	}
	return &event{
		route:   route,
		method:  req.Method,
		path:    req.URL.Path,
		query:   req.URL.RawQuery,
		headers: req.Header,
		params:  params,
		body:    body,
		remote:  req.RemoteAddr,
		host:    req.Host,
		proto:   req.Proto,
		tls:     req.TLS != nil,
		values:  middleware.Values(req.Context()),
		form:    form,
	}, cleanup, true
}

// response is PHP's answer to one request.
type response struct {
	status int
	header http.Header
	body   []byte
}

var errBadResponse = errors.New("bad php response")

// callPHP returns a value, not a pointer: uncached routes then allocate
// nothing for it.
func callPHP(ctx context.Context, php Dispatcher, ev *event) (response, error) {
	raw, body, err := php.Dispatch(ctx, ev)
	if err != nil {
		return response{}, err
	}
	status, header, err := decodeAnswer(raw)
	if err != nil {
		return response{}, fmt.Errorf("%w: %w", errBadResponse, err)
	}
	if status == 0 {
		status = http.StatusOK
	}
	return response{status: status, header: header, body: body}, nil
}

func writeError(w http.ResponseWriter, req *http.Request, err error, log *slog.Logger) {
	switch {
	case errors.Is(err, errBadResponse):
		log.Error("decode php response", "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	case errors.Is(err, context.DeadlineExceeded):
		http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
	case req.Context().Err() == nil:
		// The PHP thread died with the request (a fatal error).
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
	// Otherwise the client went away; there is nobody to answer.
}

// write sends the response. Its header may be the cache's, so the map is
// never handed over; a value slice is, because decodeAnswer left it no room
// to be appended to in place.
func (r *response) write(w http.ResponseWriter) {
	h := w.Header()
	for k, vs := range r.header {
		if old, ok := h[k]; ok {
			h[k] = append(old, vs...)
			continue
		}
		h[k] = vs
	}
	w.WriteHeader(r.status)
	_, _ = w.Write(r.body)
}

func seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }
