// Package middleware implements the Go-side middleware that server.php
// declares with $server->use(Middleware::...). It runs in Go, before a
// request reaches PHP, so rejected or compressed requests cost PHP nothing.
//
// HTTP: request_id, access_log, cors, rate_limit, max_body, gzip.
// gRPC: request_id, access_log, rate_limit.
// Middleware runs in the declared order: the first one declared is outermost.
package middleware

import (
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mohamed-samir907/cairo/internal/spec"
)

// HTTP wraps h with the declared middleware, first declared outermost.
func HTTP(h http.Handler, decl []spec.Middleware, log *slog.Logger) (http.Handler, error) {
	for i := len(decl) - 1; i >= 0; i-- {
		m := decl[i]
		var err error
		switch m.Name {
		case "request_id":
			h = requestID(h)
		case "access_log":
			h = accessLog(h, log)
		case "cors":
			var o CORSOptions
			if err = decode(m, &o); err == nil {
				h = cors(h, o)
			}
		case "rate_limit":
			var o RateOptions
			if err = decode(m, &o); err == nil {
				var l *limiter
				if l, err = newLimiter(o); err == nil {
					h = rateLimitHTTP(h, l)
				}
			}
		case "max_body":
			var o struct {
				Bytes int64 `json:"bytes"`
			}
			if err = decode(m, &o); err == nil {
				h = maxBody(h, o.Bytes)
			}
		case "gzip":
			var o struct {
				MinBytes int `json:"min_bytes"`
			}
			if err = decode(m, &o); err == nil {
				h = gzipHandler(h, o.MinBytes)
			}
		default:
			err = fmt.Errorf("unknown http middleware %q", m.Name)
		}
		if err != nil {
			return nil, fmt.Errorf("middleware %s: %w", m.Name, err)
		}
	}
	return h, nil
}

func decode(m spec.Middleware, v any) error {
	if len(m.Options) == 0 || string(m.Options) == "null" || string(m.Options) == "[]" {
		return nil
	}
	return json.Unmarshal(m.Options, v)
}

// ---- request id ----

const requestIDHeader = "X-Request-Id"

// NewRequestID returns a random 16-byte hex id.
func NewRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// requestID keeps a valid incoming X-Request-Id or sets a new one, on the
// request (so PHP sees it) and on the response.
func requestID(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if !validID(id) {
			id = NewRequestID()
			r.Header.Set(requestIDHeader, id)
		}
		w.Header().Set(requestIDHeader, id)
		h.ServeHTTP(w, r)
	})
}

func validID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

// ---- access log ----

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func accessLog(h http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		h.ServeHTTP(sw, r)
		log.Info("http", "method", r.Method, "path", r.URL.Path, "status", sw.status, "bytes", sw.bytes,
			"took", time.Since(start).Round(time.Microsecond), "remote", r.RemoteAddr, "request_id", r.Header.Get(requestIDHeader))
	})
}

// ---- CORS ----

// CORSOptions configure the cors middleware. An empty Origins allows none;
// "*" allows any.
type CORSOptions struct {
	Origins     []string `json:"origins"`
	Methods     []string `json:"methods"`
	Headers     []string `json:"headers"`
	Expose      []string `json:"expose"`
	Credentials bool     `json:"credentials"`
	MaxAge      int      `json:"max_age"`
}

func cors(h http.Handler, o CORSOptions) http.Handler {
	if len(o.Methods) == 0 {
		o.Methods = []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}
	}
	any := false
	allowed := map[string]bool{}
	for _, origin := range o.Origins {
		if origin == "*" {
			any = true
		}
		allowed[strings.ToLower(origin)] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		w.Header().Add("Vary", "Origin")
		if origin == "" || !(any || allowed[strings.ToLower(origin)]) {
			h.ServeHTTP(w, r)
			return
		}
		if any && !o.Credentials {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		} else {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		}
		if o.Credentials {
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}
		if len(o.Expose) > 0 {
			w.Header().Set("Access-Control-Expose-Headers", strings.Join(o.Expose, ", "))
		}
		// A preflight is answered here; PHP never sees it.
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			w.Header().Set("Access-Control-Allow-Methods", strings.Join(o.Methods, ", "))
			if len(o.Headers) > 0 {
				w.Header().Set("Access-Control-Allow-Headers", strings.Join(o.Headers, ", "))
			} else if req := r.Header.Get("Access-Control-Request-Headers"); req != "" {
				w.Header().Set("Access-Control-Allow-Headers", req)
			}
			if o.MaxAge > 0 {
				w.Header().Set("Access-Control-Max-Age", strconv.Itoa(o.MaxAge))
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// ---- rate limit ----

// RateOptions configure rate_limit: PerSecond tokens refill each second, up
// to Burst. By is "ip" (one bucket per client address) or "global".
type RateOptions struct {
	PerSecond float64 `json:"per_second"`
	Burst     int     `json:"burst"`
	By        string  `json:"by"`
}

type bucket struct {
	tokens float64
	last   time.Time
}

type limiter struct {
	opts    RateOptions
	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time
	sweep   time.Time
}

func newLimiter(o RateOptions) (*limiter, error) {
	if o.PerSecond <= 0 {
		return nil, fmt.Errorf("per_second must be above 0")
	}
	if o.Burst <= 0 {
		o.Burst = int(o.PerSecond)
		if o.Burst < 1 {
			o.Burst = 1
		}
	}
	switch o.By {
	case "", "ip":
		o.By = "ip"
	case "global":
	default:
		return nil, fmt.Errorf(`by must be "ip" or "global", not %q`, o.By)
	}
	return &limiter{opts: o, buckets: map[string]*bucket{}, now: time.Now}, nil
}

// allow takes a token for key; when there is none it returns false and how
// long until the next one.
func (l *limiter) allow(key string) (bool, time.Duration) {
	if l.opts.By == "global" {
		key = ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: float64(l.opts.Burst), last: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.opts.PerSecond
	if max := float64(l.opts.Burst); b.tokens > max {
		b.tokens = max
	}
	b.last = now
	// Drop full buckets now and then, so idle clients do not pile up.
	if now.Sub(l.sweep) > time.Minute {
		l.sweep = now
		for k, ob := range l.buckets {
			if k != key && now.Sub(ob.last).Seconds()*l.opts.PerSecond >= float64(l.opts.Burst) {
				delete(l.buckets, k)
			}
		}
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / l.opts.PerSecond * float64(time.Second))
}

func clientIP(remote string) string {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return remote
	}
	return host
}

func rateLimitHTTP(h http.Handler, l *limiter) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, wait := l.allow(clientIP(r.RemoteAddr))
		if !ok {
			secs := int(wait.Seconds() + 0.999)
			if secs < 1 {
				secs = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// ---- max body ----

func maxBody(h http.Handler, n int64) http.Handler {
	if n <= 0 {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > n {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, n)
		h.ServeHTTP(w, r)
	})
}

// ---- gzip ----

type gzipWriter struct {
	http.ResponseWriter
	min     int
	buf     []byte
	gz      *gzip.Writer
	status  int
	decided bool
	plain   bool
}

func (g *gzipWriter) WriteHeader(code int) {
	if g.status == 0 {
		g.status = code
	}
}

func (g *gzipWriter) Write(b []byte) (int, error) {
	if g.status == 0 {
		g.status = http.StatusOK
	}
	if !g.decided {
		g.buf = append(g.buf, b...)
		if len(g.buf) < g.min {
			return len(b), nil
		}
		g.decide()
		data := g.buf
		g.buf = nil
		if _, err := g.writeOut(data); err != nil {
			return 0, err
		}
		return len(b), nil
	}
	return g.writeOut(b)
}

func (g *gzipWriter) decide() {
	g.decided = true
	h := g.Header()
	if h.Get("Content-Encoding") != "" || g.status == http.StatusNoContent || g.status == http.StatusNotModified || len(g.buf) < g.min {
		g.plain = true
		g.ResponseWriter.WriteHeader(g.status)
		return
	}
	h.Set("Content-Encoding", "gzip")
	h.Del("Content-Length")
	g.gz = gzip.NewWriter(g.ResponseWriter)
	g.ResponseWriter.WriteHeader(g.status)
}

func (g *gzipWriter) writeOut(b []byte) (int, error) {
	if g.plain {
		return g.ResponseWriter.Write(b)
	}
	return g.gz.Write(b)
}

func (g *gzipWriter) finish() {
	if !g.decided {
		if g.status == 0 {
			g.status = http.StatusOK
		}
		g.decide()
		data := g.buf
		g.buf = nil
		if len(data) > 0 {
			_, _ = g.writeOut(data)
		}
	}
	if g.gz != nil {
		_ = g.gz.Close()
	}
}

func gzipHandler(h http.Handler, min int) http.Handler {
	if min <= 0 {
		min = 1024
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Accept-Encoding")
		if !acceptsGzip(r.Header.Get("Accept-Encoding")) || r.Method == http.MethodHead {
			h.ServeHTTP(w, r)
			return
		}
		g := &gzipWriter{ResponseWriter: w, min: min}
		defer g.finish()
		h.ServeHTTP(g, r)
	})
}

func acceptsGzip(v string) bool {
	for _, part := range strings.Split(v, ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if strings.EqualFold(strings.TrimSpace(name), "gzip") || strings.TrimSpace(name) == "*" {
			if q, ok := strings.CutPrefix(strings.TrimSpace(params), "q="); ok {
				if f, err := strconv.ParseFloat(q, 64); err == nil && f == 0 {
					return false
				}
			}
			return true
		}
	}
	return false
}

var _ io.Writer = (*gzipWriter)(nil)
