// Package httpserver serves the HTTP servers declared in server.php. Go does
// the listening, routing, timeouts and static files; only matched routes reach
// PHP.
package httpserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/cairophp/cairo/internal/mercurehub"
	"github.com/cairophp/cairo/internal/middleware"
	"github.com/cairophp/cairo/internal/servertls"
	"github.com/cairophp/cairo/internal/telemetry"
	"github.com/cairophp/cairo/internal/wire"
)

// Dispatcher sends a call to PHP and returns PHP's answer: its status and
// headers (decodeAnswer) and the response body. A large body is lent, not
// copied: release is then not nil, and frees it. Nothing of the body may be
// read after that (response.free, response.own).
type Dispatcher interface {
	DispatchLent(ctx context.Context, ev wire.Event) (meta, body []byte, release func(), err error)
}

// maxBody caps request bodies read into memory for PHP.
const maxBody = 32 << 20

var paramRe = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_]*)(\.\.\.)?\}`)

// Server is one HTTP listener, and with $http->redirectHttp() the plain
// one beside it.
type Server struct {
	srv      *http.Server
	ln       net.Listener
	redirect *redirect
	log      *slog.Logger
}

// New builds one declared HTTP server and opens its listener. With tel, its
// requests are traced and measured, as declared.
func New(decl Declaration, php Dispatcher, log *slog.Logger, tel *telemetry.Telemetry) (_ *Server, err error) {
	handler, hub, err := decl.handler(php, log, tel)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			hub.Stop() // the process has one hub: a server that does not start gives it back
		}
	}()
	srv := &http.Server{
		Handler:      handler,
		ReadTimeout:  seconds(decl.ReadTimeout),
		WriteTimeout: seconds(decl.WriteTimeout),
		ErrorLog:     slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	srv.RegisterOnShutdown(stopStreams)
	srv.RegisterOnShutdown(stopSockets)
	srv.RegisterOnShutdown(stopTopics)
	srv.RegisterOnShutdown(hub.Stop)
	var secure *servertls.TLS
	if decl.TLS != nil {
		if secure, err = servertls.New(*decl.TLS, log); err != nil {
			return nil, err
		}
		srv.TLSConfig = secure.Config
	}
	if decl.H2C {
		srv.Protocols = new(http.Protocols)
		srv.Protocols.SetHTTP1(true)
		srv.Protocols.SetUnencryptedHTTP2(true)
	}
	s := &Server{srv: srv, log: log}
	if s.ln, err = net.Listen("tcp", decl.Listen); err != nil {
		return nil, err
	}
	if decl.RedirectHTTP != "" {
		if s.redirect, err = newRedirect(decl.RedirectHTTP, s.ln.Addr(), secure, srv.ErrorLog); err != nil {
			s.ln.Close()
			return nil, err
		}
		log.Info("http redirect listening", "addr", s.redirect.ln.Addr().String(), "to", s.Addr())
	}
	return s, nil
}

// handler builds the server's router with what is around it: the files Go
// serves, the measuring, the Mercure hub (returned for the server to stop)
// and the declared middleware.
func (decl *Declaration) handler(php Dispatcher, log *slog.Logger, tel *telemetry.Telemetry) (http.Handler, *mercurehub.Hub, error) {
	mux := http.NewServeMux()
	if err := decl.handleOps(mux, tel); err != nil {
		return nil, nil, err
	}
	cache := newResponseCache(cacheMaxBytes)
	var untimed func(*http.Request) bool
	noTimeout := map[string]bool{}
	for _, r := range decl.Routes {
		pattern := r.Method + " " + r.Path
		if r.Method == "*" {
			pattern = r.Path // $http->fallback(): any method
		}
		var h http.Handler = routeHandler(r, php, cache, log, seconds(decl.WriteTimeout))
		if r.Scripts {
			s, err := openScripts(decl.Scripts, decl.ScriptsFront)
			if err != nil {
				return nil, nil, err
			}
			h = s.handler(h)
		}
		mux.Handle(pattern, h)
		if r.NoTimeout {
			noTimeout[pattern] = true
		}
	}
	if len(noTimeout) > 0 || decl.Mercure != nil {
		// The timeout runs before the route is known, so it asks.
		untimed = func(req *http.Request) bool {
			if decl.isMercure(req) {
				return true
			}
			_, pattern := mux.Handler(req)
			return noTimeout[pattern]
		}
	}
	for _, s := range decl.Static {
		prefix := "/" + strings.Trim(s.Prefix, "/") + "/"
		mux.Handle("GET "+prefix, http.StripPrefix(prefix, http.FileServer(http.Dir(s.Dir))))
	}
	var routes http.Handler = mux
	if len(decl.Sendfile) > 0 {
		files, err := openSentFiles(decl.Sendfile, log)
		if err != nil {
			return nil, nil, err
		}
		routes = files.handler(mux)
	}
	if decl.PublicDir != "" {
		var err error
		if routes, err = publicFiles(decl.PublicDir, mux); err != nil {
			return nil, nil, err
		}
	}
	routes = tel.HTTP(routes, decl.isOps)
	var hub *mercurehub.Hub
	if decl.Mercure != nil {
		var err error
		if hub, err = mercurehub.Start(*decl.Mercure, log, tel.Registry()); err != nil {
			return nil, nil, err
		}
		routes = hub.Routes(routes)
	}
	handler, err := middleware.HTTP(routes, decl.Middleware, log, untimed)
	if err != nil {
		hub.Stop()
		return nil, nil, err
	}
	return handler, hub, nil
}

// Addr is the address the server listens on.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Serve blocks until the server stops.
func (s *Server) Serve() error {
	errs := make(chan error, 2) // room for both, so neither goroutine outlives its listener
	serving := 1
	go func() { errs <- s.serve() }()
	if s.redirect != nil {
		serving++
		go func() { errs <- s.redirect.serve() }()
	}
	for ; serving > 0; serving-- {
		if err := <-errs; !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	return nil
}

func (s *Server) serve() error {
	if s.srv.TLSConfig != nil {
		return s.srv.ServeTLS(s.ln, "", "") // the certificate is the config's
	}
	return s.srv.Serve(s.ln)
}

// Shutdown stops accepting connections and waits for in-flight requests.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.redirect != nil {
		_ = s.redirect.srv.Shutdown(ctx) // its requests are answered at once
	}
	return s.srv.Shutdown(ctx)
}

// event is a request as PHP receives it (Cairo\Http\Request). It is also
// the context of its call: the request's own, with the client that PHP may
// answer in pieces (stream.go).
type event struct {
	context.Context
	w      http.ResponseWriter
	mu     sync.Mutex    // holds the handler back while an operation of PHP writes (writerOf)
	ended  bool          // the handler is returning: w is not to be used
	began  began         // what PHP made of the client instead of answering: a stream, a WebSocket
	req    *http.Request // what a WebSocket's handshake reads (websocket.go)
	stall  time.Duration // the server's write timeout, which a stream gives each write
	upload *bodyStream   // the request's body, when PHP reads it in pieces (bodystream.go)

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
	script  *script // the script of a request to $http->scripts() (scripts.go)
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
	if ev.script != nil {
		n++
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
	b = wire.AppendString(wire.AppendKey(b, "remote"), ev.remote)
	b = wire.AppendString(wire.AppendKey(b, "host"), ev.host)
	b = wire.AppendString(wire.AppendKey(b, "proto"), ev.proto)
	b = wire.AppendBool(wire.AppendKey(b, "tls"), ev.tls)
	if len(ev.values) > 0 {
		b = wire.AppendValue(wire.AppendKey(b, "values"), ev.values)
	}
	if sc := ev.script; sc != nil {
		b = wire.AppendMap(wire.AppendKey(b, "script"), 5)
		b = wire.AppendString(wire.AppendKey(b, "file"), sc.file)
		b = wire.AppendString(wire.AppendKey(b, "name"), sc.name)
		b = wire.AppendString(wire.AppendKey(b, "path_info"), sc.pathInfo)
		b = wire.AppendString(wire.AppendKey(b, "root"), sc.dir)
		b = wire.AppendString(wire.AppendKey(b, "port"), sc.port)
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
	// The body is last and not in b: Tail.
	return wire.AppendTail(wire.AppendKey(b, "body"), len(ev.body))
}

// Tail is the request's body, which the engine copies behind the event
// (wire.TailEvent).
func (ev *event) Tail() []byte { return ev.body }

func routeHandler(r Route, php Dispatcher, cache *responseCache, log *slog.Logger, stall time.Duration) http.Handler {
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
		ev, cleanup, ok := newEvent(w, req, r, names)
		if !ok {
			if stale != nil {
				cache.release(key)
			}
			return
		}
		ev.name, ev.stall = name, stall
		defer cleanup()
		if !cached {
			resp, err := callPHP(ev, php, ev)
			if st := ev.end(); st != nil {
				resp.free()
				st.finish(err != nil || resp.status >= http.StatusInternalServerError)
				return
			}
			if err != nil {
				writeError(w, req, err, log)
				return
			}
			defer resp.free()
			resp.write(w)
			return
		}
		call := func(ctx context.Context) (*response, error) {
			r, err := callPHP(ctx, php, ev)
			r.own() // the cache and the requests that share the answer keep it
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
func newEvent(w http.ResponseWriter, req *http.Request, route Route, names []string) (*event, func(), bool) {
	if route.StreamBody {
		ev := &event{upload: openBody(w, req)}
		ev.fill(w, req, route.ID, pathParams(req, names))
		return ev, func() {}, true
	}
	body, err := readBody(w, req)
	if err != nil {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return nil, nil, false
	}
	cleanup := func() {}
	var form *multipartForm
	if !route.RawBody && isMultipart(req.Header) {
		form, err = parseMultipart(body, req.Header.Get("Content-Type"), os.TempDir())
		if err != nil {
			http.Error(w, "malformed multipart body", http.StatusBadRequest)
			return nil, nil, false
		}
		cleanup = form.cleanup
		body = nil // like PHP, php://input is empty for multipart/form-data
	}
	ev := &event{body: body, form: form}
	ev.fill(w, req, route.ID, pathParams(req, names))
	return ev, cleanup, true
}

// pathParams are the values of the route's parameters in req's path.
func pathParams(req *http.Request, names []string) map[string]string {
	if len(names) == 0 {
		return nil // crosses as an empty array, as a route without parameters has
	}
	params := make(map[string]string, len(names))
	for _, n := range names {
		params[n] = req.PathValue(n)
	}
	return params
}

// fill sets what an event has of its request apart from the body.
func (ev *event) fill(w http.ResponseWriter, req *http.Request, route int, params map[string]string) {
	ev.Context, ev.w, ev.req = req.Context(), w, req
	ev.route, ev.params = route, params
	ev.method, ev.path, ev.query = req.Method, req.URL.Path, req.URL.RawQuery
	ev.headers, ev.host, ev.proto = req.Header, req.Host, req.Proto
	ev.remote, ev.tls = req.RemoteAddr, req.TLS != nil
	ev.values = middleware.Values(req.Context())
	ev.script, _ = req.Context().Value(scriptKey{}).(*script)
}

// response is PHP's answer to one request. With release, its body is lent
// (Dispatcher): the request frees it when it has written it, and what
// outlives the request takes a copy first.
type response struct {
	status  int
	header  http.Header
	body    []byte
	release func()
}

// free ends the use of a lent body.
func (r *response) free() {
	if r.release != nil {
		r.release()
		r.body, r.release = nil, nil
	}
}

// own makes the body the response's own, so that it can be kept.
func (r *response) own() {
	if r.release != nil {
		body := bytes.Clone(r.body)
		r.free()
		r.body = body
	}
}

var errBadResponse = errors.New("bad php response")

// callPHP returns a value, not a pointer: uncached routes then allocate
// nothing for it.
func callPHP(ctx context.Context, php Dispatcher, ev *event) (response, error) {
	raw, body, release, err := php.DispatchLent(ctx, ev)
	if err != nil {
		return response{}, err
	}
	status, header, err := decodeAnswer(raw)
	if err != nil {
		if release != nil {
			release()
		}
		return response{}, fmt.Errorf("%w: %w", errBadResponse, err)
	}
	if status == 0 {
		status = http.StatusOK
	}
	return response{status: status, header: header, body: body, release: release}, nil
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
