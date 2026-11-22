package httpserver

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cairophp/cairo/internal/wire"
)

// lendingPHP answers with a body that it lends, as the engine does a large
// one: once released, the body is overwritten, as freed memory may be.
type lendingPHP struct {
	meta     []byte
	lent     atomic.Int64
	released atomic.Int64
}

const lentText = "the body PHP answered with"

func (p *lendingPHP) DispatchLent(context.Context, wire.Event) ([]byte, []byte, func(), error) {
	body := []byte(lentText)
	p.lent.Add(1)
	meta := p.meta
	if meta == nil {
		meta = wire.AppendHeader(wire.AppendInt(nil, 200), nil)
	}
	return meta, body, func() {
		p.released.Add(1)
		copy(body, strings.Repeat("!", len(body)))
	}, nil
}

func (p *lendingPHP) check(t *testing.T) {
	t.Helper()
	if l, r := p.lent.Load(), p.released.Load(); l != r {
		t.Errorf("%d bodies lent, %d released", l, r)
	}
}

func TestLentBodyIsWrittenThenReleased(t *testing.T) {
	for _, method := range []string{"GET", "HEAD", "POST"} {
		php := &lendingPHP{}
		h := routeHandler(Route{ID: 1, Method: method, Path: "/"}, php, nil, slog.Default())
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, "/", nil))
		if got := w.Body.String(); got != lentText {
			t.Errorf("%s: the client got %q", method, got)
		}
		php.check(t)
	}
}

// The cache keeps a response past the request that made it, and so do the
// requests that waited for the same answer: they need a copy of a lent body.
func TestLentBodyIsCopiedForTheCache(t *testing.T) {
	php := &lendingPHP{}
	h := routeHandler(Route{ID: 1, Method: "GET", Path: "/", Cache: &RouteCache{TTL: 60}}, php, newResponseCache(cacheMaxBytes), slog.Default())
	for range 3 {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
		if got := w.Body.String(); got != lentText {
			t.Fatalf("the client got %q", got)
		}
	}
	if n := php.lent.Load(); n != 1 {
		t.Fatalf("PHP ran %d times, want 1", n)
	}
	php.check(t)
}

func TestLentBodyIsReleasedWhenTheAnswerIsMalformed(t *testing.T) {
	php := &lendingPHP{meta: []byte{0xff}}
	h := routeHandler(Route{ID: 1, Method: "GET", Path: "/"}, php, nil, slog.New(slog.DiscardHandler))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), lentText) {
		t.Fatalf("got %d %q, want 500 without PHP's body", w.Code, w.Body.String())
	}
	php.check(t)
}

// A handler that panics while it writes must not keep the body.
func TestLentBodyIsReleasedWhenTheWriteFails(t *testing.T) {
	php := &lendingPHP{}
	h := routeHandler(Route{ID: 1, Method: "GET", Path: "/"}, php, nil, slog.Default())
	func() {
		defer func() { _ = recover() }()
		h.ServeHTTP(panicWriter{httptest.NewRecorder()}, httptest.NewRequest("GET", "/", nil))
	}()
	php.check(t)
}

type panicWriter struct{ http.ResponseWriter }

func (panicWriter) Write([]byte) (int, error) { panic(http.ErrAbortHandler) }
