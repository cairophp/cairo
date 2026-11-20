package httpserver

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cairophp/cairo/internal/cachetag"
	"github.com/cairophp/cairo/internal/wire"
)

// fakePHP answers every call with the call's number as the body, after delay.
type fakePHP struct {
	calls  atomic.Int64
	delay  time.Duration
	header http.Header
}

func (f *fakePHP) Dispatch(ctx context.Context, _ wire.Event) ([]byte, []byte, error) {
	n := f.calls.Add(1)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
	return wire.AppendHeader(wire.AppendInt(nil, 200), f.header), []byte(strconv.FormatInt(n, 10)), nil
}

func cachedRoute(t *testing.T, php *fakePHP, c RouteCache) func(req *http.Request) string {
	t.Helper()
	h := routeHandler(Route{ID: 1, Method: "GET", Path: "/", Cache: &c}, php, newResponseCache(cacheMaxBytes), slog.Default())
	return func(req *http.Request) string {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		b, _ := io.ReadAll(w.Result().Body)
		return string(b)
	}
}

func get(target string, header ...string) *http.Request {
	req := httptest.NewRequest("GET", target, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Add(header[i], header[i+1])
	}
	return req
}

func TestCacheServesRepeats(t *testing.T) {
	php := &fakePHP{}
	do := cachedRoute(t, php, RouteCache{TTL: 60})
	for range 3 {
		if got := do(get("/?a=1")); got != "1" {
			t.Fatalf("got %q, want the first answer", got)
		}
	}
	if got := do(get("/?a=2")); got != "2" {
		t.Fatalf("another query got %q, want its own answer", got)
	}
	if got := do(httptest.NewRequest("POST", "/?a=1", nil)); got != "3" {
		t.Fatalf("POST got %q, want PHP to run", got)
	}
}

func TestCacheSkipsPersonalRequests(t *testing.T) {
	php := &fakePHP{}
	do := cachedRoute(t, php, RouteCache{TTL: 60, Vary: []string{"cookie"}})
	do(get("/"))
	if got := do(get("/", "Authorization", "Bearer x")); got != "2" {
		t.Fatalf("Authorization request got %q, want its own answer", got)
	}
	if a, b := do(get("/", "Cookie", "s=a")), do(get("/", "Cookie", "s=b")); a == b {
		t.Fatalf("two cookies shared answer %q on a route that varies on Cookie", a)
	}
	if got := do(get("/", "Cookie", "s=a")); got != "3" {
		t.Fatalf("repeat of a varied request got %q, want 3", got)
	}
}

func TestCacheNeverStoresSetCookie(t *testing.T) {
	for _, h := range []http.Header{
		{"set-cookie": {"s=1"}},
		{"cache-control": {"Private, max-age=5"}},
		{"Cache-Control": {"no-store"}},
	} {
		php := &fakePHP{header: h, delay: 50 * time.Millisecond}
		do := cachedRoute(t, php, RouteCache{TTL: 60})
		var wg sync.WaitGroup
		for range 10 {
			wg.Add(1)
			go func() { defer wg.Done(); do(get("/")) }()
		}
		wg.Wait()
		if n := php.calls.Load(); n != 10 {
			t.Errorf("%v: PHP ran %d times for 10 requests, want 10 (nothing shared)", h, n)
		}
	}
}

// Identical requests that arrive together make one PHP call.
func TestCacheMergesInFlightRequests(t *testing.T) {
	php := &fakePHP{delay: 200 * time.Millisecond}
	do := cachedRoute(t, php, RouteCache{TTL: 60})
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := do(get("/")); got != "1" {
				t.Errorf("got %q, want the one answer", got)
			}
		}()
	}
	wg.Wait()
	if n := php.calls.Load(); n != 1 {
		t.Fatalf("PHP ran %d times for 50 identical requests, want 1", n)
	}
}

// After the TTL a stale answer is served at once while one call rebuilds it.
func TestCacheStaleWhileRevalidate(t *testing.T) {
	php := &fakePHP{delay: 100 * time.Millisecond}
	do := cachedRoute(t, php, RouteCache{TTL: 0.05, StaleWhileRevalidate: 60})
	do(get("/"))
	time.Sleep(80 * time.Millisecond) // past the TTL
	start := time.Now()
	for range 5 {
		if got := do(get("/")); got != "1" {
			t.Fatalf("stale request got %q, want the stale answer", got)
		}
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("stale requests waited %v for the rebuild", d)
	}
	deadline := time.Now().Add(2 * time.Second)
	for do(get("/")) != "2" {
		if time.Now().After(deadline) {
			t.Fatal("the stale entry was never rebuilt")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := php.calls.Load(); n != 2 {
		t.Fatalf("PHP ran %d times, want 2 (one rebuild)", n)
	}
}

func TestCacheExpires(t *testing.T) {
	php := &fakePHP{}
	do := cachedRoute(t, php, RouteCache{TTL: 0.02})
	do(get("/"))
	time.Sleep(40 * time.Millisecond)
	if got := do(get("/")); got != "2" {
		t.Fatalf("expired entry served: got %q", got)
	}
}

func TestCacheEvictsPastItsSize(t *testing.T) {
	c := newResponseCache(10 * entryOverhead)
	p := &cachePolicy{ttl: time.Minute}
	for i := range 100 {
		c.mu.Lock()
		c.store(strconv.Itoa(i), &response{status: 200, body: make([]byte, 100)}, p, 0)
		c.mu.Unlock()
	}
	if c.bytes > c.maxBytes || len(c.entries) != c.lru.Len() || len(c.entries) == 0 {
		t.Fatalf("bytes %d of %d, %d entries, %d in LRU", c.bytes, c.maxBytes, len(c.entries), c.lru.Len())
	}
	if resp, _ := c.lookup("99", p, true); resp == nil {
		t.Fatal("the newest entry was evicted")
	}
	if resp, _ := c.lookup("0", p, true); resp != nil {
		t.Fatal("the oldest entry survived")
	}
}

// A purge drops the tagged routes' entries, and only theirs.
func TestCachePurgeByTag(t *testing.T) {
	php := &fakePHP{}
	products := cachedRoute(t, php, RouteCache{TTL: 60, StaleWhileRevalidate: 60, Tags: []string{"t-products", "t-shop"}})
	users := cachedRoute(t, php, RouteCache{TTL: 60, Tags: []string{"t-users"}})
	products(get("/"))
	users(get("/"))
	cachetag.Purge("t-products")
	if got := products(get("/")); got != "3" {
		t.Fatalf("purged route got %q, want a new answer (3)", got)
	}
	if got := users(get("/")); got != "2" {
		t.Fatalf("route with another tag got %q, want its cached answer (2)", got)
	}
	if got := products(get("/")); got != "3" {
		t.Fatalf("after the purge got %q, want the new answer cached (3)", got)
	}
}

// An answer PHP started building before a purge is neither stored nor handed
// to requests that arrive after the purge.
func TestCachePurgeDuringCall(t *testing.T) {
	php := &fakePHP{delay: 150 * time.Millisecond}
	do := cachedRoute(t, php, RouteCache{TTL: 60, Tags: []string{"t-during"}})
	first := make(chan string)
	go func() { first <- do(get("/")) }()
	for php.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	cachetag.Purge("t-during")
	if got := do(get("/")); got != "2" {
		t.Fatalf("request after the purge got %q, want its own answer (2)", got)
	}
	if got := <-first; got != "1" {
		t.Fatalf("request before the purge got %q, want 1", got)
	}
	if got := do(get("/")); got != "2" {
		t.Fatalf("got %q: the answer from before the purge was stored", got)
	}
}

// HEAD reads what GET stored and never fills the cache itself.
func TestCacheHead(t *testing.T) {
	php := &fakePHP{}
	do := cachedRoute(t, php, RouteCache{TTL: 60})
	head := func() { do(httptest.NewRequest("HEAD", "/", nil)) }
	head()
	head()
	if n := php.calls.Load(); n != 2 {
		t.Fatalf("PHP ran %d times for two HEAD misses, want 2", n)
	}
	if got := do(get("/")); got != "3" {
		t.Fatalf("GET after HEAD got %q, want PHP to run: HEAD must not fill the cache", got)
	}
	head()
	if n := php.calls.Load(); n != 3 {
		t.Fatalf("PHP ran %d times, want 3: HEAD after GET is a cache hit", n)
	}
}
