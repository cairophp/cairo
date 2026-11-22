package httpserver

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cairophp/cairo/internal/wire"
)

type constPHP struct{}

func (constPHP) DispatchLent(context.Context, wire.Event) ([]byte, []byte, func(), error) {
	return wire.AppendHeader(wire.AppendInt(nil, 200), http.Header(nil)), []byte("1"), nil, nil
}

func benchRoute(b *testing.B, c *RouteCache) {
	h := routeHandler(Route{ID: 1, Method: "GET", Path: "/", Cache: c}, constPHP{}, newResponseCache(cacheMaxBytes), slog.Default())
	req := httptest.NewRequest("GET", "/", nil)
	b.ReportAllocs()
	for b.Loop() {
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
}

func BenchmarkUncachedRoute(b *testing.B) { benchRoute(b, nil) }
func BenchmarkCacheHit(b *testing.B)      { benchRoute(b, &RouteCache{TTL: 60}) }
