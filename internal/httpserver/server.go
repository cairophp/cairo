// Package httpserver serves the HTTP servers declared in server.php. Go does
// the listening, routing, timeouts and static files; only matched routes reach
// PHP.
package httpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/mohamed-samir907/cairo/internal/middleware"
	"github.com/mohamed-samir907/cairo/internal/spec"
)

// Dispatcher sends a call to PHP and returns PHP's JSON response.
type Dispatcher interface {
	Dispatch(ctx context.Context, ev any) ([]byte, error)
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

// event is a request as PHP receives it (Cairo\Http\Request). A struct, not
// a map: it encodes about four times faster.
type event struct {
	Type    string            `json:"type"`
	Route   int               `json:"route"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Query   string            `json:"query"`
	Headers http.Header       `json:"headers"`
	Params  map[string]string `json:"params"`
	Body    string            `json:"body"`
	Remote  string            `json:"remote"`
	Host    string            `json:"host"`
	Proto   string            `json:"proto"`
	TLS     bool              `json:"tls"`
	Values  map[string]any    `json:"values,omitempty"`
	Form    []formField       `json:"form,omitempty"`
	Files   []uploadedFile    `json:"files,omitempty"`
}

type phpResponse struct {
	Status  int                 `json:"status"`
	Headers map[string][]string `json:"headers"`
	Body    string              `json:"body"`
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
			Type:    "http",
			Route:   r.ID,
			Method:  req.Method,
			Path:    req.URL.Path,
			Query:   req.URL.RawQuery,
			Headers: req.Header,
			Params:  params,
			Body:    base64.StdEncoding.EncodeToString(body),
			Remote:  req.RemoteAddr,
			Host:    req.Host,
			Proto:   req.Proto,
			TLS:     req.TLS != nil,
			Values:  middleware.Values(req.Context()),
		}
		if form != nil {
			ev.Form = form.fields
			ev.Files = form.files
		}
		raw, err := php.Dispatch(req.Context(), ev)
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
		var resp phpResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			log.Error("decode php response", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		out, err := base64.StdEncoding.DecodeString(resp.Body)
		if err != nil {
			log.Error("decode php response body", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		for k, vs := range resp.Headers {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		if resp.Status == 0 {
			resp.Status = http.StatusOK
		}
		w.WriteHeader(resp.Status)
		_, _ = w.Write(out)
	})
}

func seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }
