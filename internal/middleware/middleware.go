// Package middleware implements the Go-side middleware that server.php
// declares with $server->use(Middleware::...). It runs in Go, before a
// request reaches PHP, so a rejected request costs PHP nothing.
//
// HTTP: request_id, access_log, max_body, timeout.
// gRPC: request_id, access_log, timeout.
// Middleware runs in the declared order: the first one declared is outermost.
package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// Declaration is one `$server->use(Middleware::...)`: Go runs it before PHP.
type Declaration struct {
	Name    string          `json:"name"`
	Options json.RawMessage `json:"options"`
}

// HTTP wraps h with the declared middleware, first declared outermost.
func HTTP(h http.Handler, decl []Declaration, log *slog.Logger) (http.Handler, error) {
	for i := len(decl) - 1; i >= 0; i-- {
		m := decl[i]
		var err error
		switch m.Name {
		case "request_id":
			h = requestID(h)
		case "access_log":
			h = accessLog(h, log)
		case "max_body":
			var o struct {
				Bytes int64 `json:"bytes"`
			}
			if err = decode(m, &o); err == nil {
				h = maxBody(h, o.Bytes)
			}
		case "timeout":
			var d time.Duration
			if d, err = timeoutOf(m); err == nil {
				h = timeoutHTTP(h, d)
			}
		default:
			err = fmt.Errorf("unknown http middleware %q", m.Name)
		}
		if err != nil {
			return nil, fmt.Errorf("middleware %s: %w", m.Name, err)
		}
	}
	return recoverHTTP(h, log), nil
}

// timeoutOf reads the timeout middleware's "seconds".
func timeoutOf(m Declaration) (time.Duration, error) {
	var o struct {
		Seconds float64 `json:"seconds"`
	}
	if err := decode(m, &o); err != nil {
		return 0, err
	}
	if o.Seconds <= 0 {
		return 0, fmt.Errorf("seconds must be positive")
	}
	return time.Duration(o.Seconds * float64(time.Second)), nil
}

// timeoutHTTP gives each request a deadline; Dispatch answers 504 past it.
func timeoutHTTP(h http.Handler, d time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

func decode(m Declaration, v any) error {
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
		h.ServeHTTP(w, r.WithContext(WithValue(r.Context(), "request_id", id)))
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
