// Package httpserver serves the HTTP servers declared in server.php. Go does
// the listening, routing, timeouts and static files; only matched routes reach
// PHP.
package httpserver

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/cairophp/cairo/internal/middleware"
	"github.com/cairophp/cairo/internal/spec"
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
func New(decl spec.HTTPServer, php Dispatcher, log *slog.Logger) (*Server, error) {
	mux := http.NewServeMux()
	for _, r := range decl.Routes {
		pattern := r.Method + " " + r.Path
		if r.Method == "*" {
			pattern = r.Path // $http->fallback(): any method
		}
		mux.Handle(pattern, routeHandler(r, php, log))
	}
	for _, s := range decl.Static {
		prefix := "/" + strings.Trim(s.Prefix, "/") + "/"
		mux.Handle("GET "+prefix, http.StripPrefix(prefix, http.FileServer(http.Dir(s.Dir))))
	}
	handler, err := middleware.HTTP(mux, decl.Middleware, log)
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

func routeHandler(r spec.Route, php Dispatcher, log *slog.Logger) http.Handler {
	var names []string
	for _, m := range paramRe.FindAllStringSubmatch(r.Path, -1) {
		names = append(names, m[1])
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxBody))
		if err != nil {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		params := make(map[string]string, len(names))
		for _, n := range names {
			params[n] = req.PathValue(n)
		}
		var form *multipartForm
		if strings.HasPrefix(strings.ToLower(req.Header.Get("Content-Type")), "multipart/form-data") {
			form, err = parseMultipart(body, req.Header.Get("Content-Type"), os.TempDir())
			if err != nil {
				http.Error(w, "malformed multipart body", http.StatusBadRequest)
				return
			}
			defer form.cleanup()
			body = nil // like PHP, php://input is empty for multipart/form-data
		}
		ev := &event{
			route:   r.ID,
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
		}
		raw, out, err := php.Dispatch(req.Context(), ev)
		if err != nil {
			switch {
			case errors.Is(err, context.DeadlineExceeded):
				http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
			case req.Context().Err() == nil:
				// The PHP thread died with the request (a fatal error).
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
			// Otherwise the client went away; there is nobody to answer.
			return
		}
		status, header, err := wire.DecodeResponse(raw)
		if err != nil {
			log.Error("decode php response", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		h := w.Header()
		for k, vs := range header {
			h[http.CanonicalHeaderKey(k)] = append(h[http.CanonicalHeaderKey(k)], vs...)
		}
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = w.Write(out)
	})
}

func seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }
