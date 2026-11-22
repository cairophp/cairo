package httpserver

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cairophp/cairo/internal/wire"
)

// apiPHP answers as an API route would: it encodes the event, as the engine
// does for the PHP thread, and returns five headers.
type apiPHP struct {
	buf    []byte
	answer []byte
}

func newAPIPHP() *apiPHP {
	return &apiPHP{answer: wire.AppendHeader(wire.AppendInt(nil, 200), http.Header{
		"Content-Type":          {"application/json"},
		"Cache-Control":         {"private, no-store"},
		"X-Request-Id":          {"5f0c1d2e-8a7b-4c3d-9e1f-0a1b2c3d4e5f"},
		"X-RateLimit-Remaining": {"59"},
		"Vary":                  {"Accept-Encoding, Authorization"},
	})}
}

func (p *apiPHP) DispatchLent(_ context.Context, ev wire.Event) ([]byte, []byte, func(), error) {
	p.buf = ev.AppendWire(p.buf[:0], 1, 0)
	return p.answer, []byte(`{"ok":true}`), nil, nil
}

// browserRequest is a GET with the headers a browser and a proxy send.
func browserRequest() *http.Request {
	req := httptest.NewRequest("GET", "/api/items/42?page=2&sort=name", nil)
	for k, v := range map[string]string{
		"User-Agent":        "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
		"Accept":            "application/json, text/plain, */*",
		"Accept-Language":   "en-US,en;q=0.9,ar;q=0.8",
		"Accept-Encoding":   "gzip, deflate, br",
		"Authorization":     "Bearer placeholder",
		"Cookie":            "sid=0123456789abcdef; theme=dark; locale=en",
		"Referer":           "https://app.example.com/items?page=2",
		"Origin":            "https://app.example.com",
		"X-Request-Id":      "5f0c1d2e-8a7b-4c3d-9e1f-0a1b2c3d4e5f",
		"X-Forwarded-For":   "203.0.113.7, 10.0.0.12",
		"X-Forwarded-Proto": "https",
		"Sec-Fetch-Mode":    "cors",
		"Sec-Fetch-Site":    "same-site",
	} {
		req.Header.Set(k, v)
	}
	return req
}

// A request as an API gets it, through the route's handler and back: what
// Cairo's own code costs between net/http and the PHP thread.
func BenchmarkAPIRoute(b *testing.B) {
	mux := http.NewServeMux()
	mux.Handle("GET /api/items/{id}", routeHandler(Route{ID: 1, Method: "GET", Path: "/api/items/{id}"}, newAPIPHP(), newResponseCache(cacheMaxBytes), slog.Default()))
	req := browserRequest()
	b.ReportAllocs()
	for b.Loop() {
		mux.ServeHTTP(httptest.NewRecorder(), req)
	}
}
