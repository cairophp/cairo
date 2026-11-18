package httpserver

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cairophp/cairo/internal/spec"
	"github.com/cairophp/cairo/internal/wire"
)

type constPHP struct{}

func (constPHP) Dispatch(context.Context, wire.Event) ([]byte, []byte, error) {
	return wire.AppendHeader(wire.AppendInt(nil, 200), http.Header(nil)), []byte("1"), nil
}

func benchRoute(b *testing.B, c *spec.RouteCache) {
	h := routeHandler(spec.Route{ID: 1, Method: "GET", Path: "/", Cache: c}, constPHP{}, newResponseCache(cacheMaxBytes), slog.Default())
	req := httptest.NewRequest("GET", "/", nil)
	b.ReportAllocs()
	for b.Loop() {
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
}

func BenchmarkUncachedRoute(b *testing.B) { benchRoute(b, nil) }
func BenchmarkCacheHit(b *testing.B)      { benchRoute(b, &spec.RouteCache{TTL: 60}) }
