package httpserver

import (
	"container/list"
	"context"
	"net/http"
	"net/textproto"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cairophp/cairo/internal/spec"
)

// Limits of one server's response cache. An entry costs its key, headers
// and body plus a fixed overhead, so many tiny entries are bounded too.
const (
	cacheMaxBytes = 64 << 20
	cacheMaxEntry = 1 << 20
	entryOverhead = 256
	// refreshTimeout bounds a background revalidation, which has no client
	// whose deadline it could inherit.
	refreshTimeout = 60 * time.Second
)

// cachePolicy is one route's ->cache(...): how long its responses stay
// fresh, how long after that a stale one is served while it is rebuilt, and
// which request headers it varies on.
type cachePolicy struct {
	route int
	ttl   time.Duration
	stale time.Duration
	vary  []string // canonical names, sorted
}

func newCachePolicy(route int, c spec.RouteCache) *cachePolicy {
	vary := make([]string, 0, len(c.Vary))
	for _, h := range c.Vary {
		vary = append(vary, textproto.CanonicalMIMEHeaderKey(h))
	}
	slices.Sort(vary)
	return &cachePolicy{route: route, ttl: seconds(c.TTL), stale: seconds(c.StaleWhileRevalidate), vary: slices.Compact(vary)}
}

// key is the cache key of req, or false when req must not use the cache:
// only GET is cached, and a request carrying credentials is personal unless
// the route varies on that header.
func (p *cachePolicy) key(req *http.Request) (string, bool) {
	if p == nil || req.Method != http.MethodGet {
		return "", false
	}
	for _, h := range [...]string{"Authorization", "Cookie"} {
		if _, ok := req.Header[h]; ok && !slices.Contains(p.vary, h) {
			return "", false
		}
	}
	// Length-prefixed fields, so no two requests share a key. The raw query
	// is used as PHP sees it: a parsed one would drop malformed pairs.
	b := strconv.AppendInt(make([]byte, 0, 128), int64(p.route), 10)
	b = appendField(b, req.Host)
	b = appendField(b, req.URL.Path)
	b = appendField(b, req.URL.RawQuery)
	for _, h := range p.vary {
		vs := req.Header[h]
		b = strconv.AppendInt(append(b, ' '), int64(len(vs)), 10)
		for _, v := range vs {
			b = appendField(b, v)
		}
	}
	return string(b), true
}

func appendField(b []byte, s string) []byte {
	b = strconv.AppendInt(append(b, ' '), int64(len(s)), 10)
	return append(append(b, ':'), s...)
}

// cacheable reports whether r may be served to other clients.
func cacheable(r *response) bool {
	if r.status != http.StatusOK || len(r.body) > cacheMaxEntry {
		return false
	}
	if len(values(r.header, "Set-Cookie")) > 0 || slices.Contains(values(r.header, "Vary"), "*") {
		return false
	}
	cc := strings.ToLower(strings.Join(values(r.header, "Cache-Control"), ","))
	return !strings.Contains(cc, "private") && !strings.Contains(cc, "no-store") && !strings.Contains(cc, "no-cache")
}

// values returns every value of header name: PHP's header names are not
// canonical, and a missed Set-Cookie would leak one client's session.
func values(h http.Header, name string) []string {
	var out []string
	for k, vs := range h {
		if strings.EqualFold(k, name) {
			out = append(out, vs...)
		}
	}
	return out
}

type cacheEntry struct {
	key        string
	resp       *response
	size       int
	fresh      time.Time // served as is until then
	expires    time.Time // served stale, and rebuilt, until then
	refreshing bool
}

// flight is a PHP call that identical requests wait for instead of making
// their own. resp stays nil when the answer may not be shared.
type flight struct {
	done chan struct{}
	resp *response
}

// responseCache is a bounded LRU of PHP responses, shared by the routes of
// one server.
type responseCache struct {
	mu       sync.Mutex
	entries  map[string]*list.Element
	lru      list.List // front: most recently used *cacheEntry
	bytes    int
	maxBytes int
	flights  map[string]*flight
}

func newResponseCache(maxBytes int) *responseCache {
	return &responseCache{entries: map[string]*list.Element{}, flights: map[string]*flight{}, maxBytes: maxBytes}
}

type callFunc func(ctx context.Context) (*response, error)

// serve answers req from the cache, or through call when it has no usable
// entry. A stale entry is served at once and rebuilt by one background call.
func (c *responseCache) serve(req *http.Request, key string, p *cachePolicy, call callFunc) (*response, error) {
	resp, refresh := c.lookup(key)
	if resp == nil {
		return c.fetch(req.Context(), key, p, call)
	}
	if refresh {
		go func() {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(req.Context()), refreshTimeout)
			defer cancel()
			_, _ = c.fetch(ctx, key, p, call)
		}()
	}
	return resp, nil
}

// lookup returns the entry for key if it is fresh or stale, and whether the
// caller should start its refresh (true once per stale entry).
func (c *responseCache) lookup(key string) (*response, bool) {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	e := el.Value.(*cacheEntry)
	if !now.Before(e.expires) {
		c.remove(el)
		return nil, false
	}
	c.lru.MoveToFront(el)
	if now.Before(e.fresh) || e.refreshing {
		return e.resp, false
	}
	e.refreshing = true
	return e.resp, true
}

// fetch runs call, unless an identical call is already running: then it
// waits for that one and shares its answer when the answer is cacheable.
func (c *responseCache) fetch(ctx context.Context, key string, p *cachePolicy, call callFunc) (*response, error) {
	c.mu.Lock()
	if f, ok := c.flights[key]; ok {
		c.mu.Unlock()
		select {
		case <-f.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if f.resp != nil {
			return f.resp, nil
		}
		return call(ctx) // not shareable: this client gets its own answer
	}
	f := &flight{done: make(chan struct{})}
	c.flights[key] = f
	c.mu.Unlock()

	resp, err := call(ctx)
	c.mu.Lock()
	delete(c.flights, key)
	if err == nil && cacheable(resp) {
		f.resp = resp
		c.store(key, resp, p)
	} else if el, ok := c.entries[key]; ok {
		el.Value.(*cacheEntry).refreshing = false // let a later request retry
	}
	c.mu.Unlock()
	close(f.done)
	return resp, err
}

// store adds or replaces the entry for key and evicts the least recently
// used ones past the size limit. c.mu is held.
func (c *responseCache) store(key string, r *response, p *cachePolicy) {
	if el, ok := c.entries[key]; ok {
		c.remove(el)
	}
	size := len(key) + len(r.body) + entryOverhead
	for k, vs := range r.header {
		size += len(k)
		for _, v := range vs {
			size += len(v)
		}
	}
	if size > c.maxBytes {
		return
	}
	now := time.Now()
	e := &cacheEntry{key: key, resp: r, size: size, fresh: now.Add(p.ttl), expires: now.Add(p.ttl + p.stale)}
	c.entries[key] = c.lru.PushFront(e)
	c.bytes += size
	for c.bytes > c.maxBytes {
		c.remove(c.lru.Back())
	}
}

// remove drops one entry. c.mu is held.
func (c *responseCache) remove(el *list.Element) {
	e := c.lru.Remove(el).(*cacheEntry)
	delete(c.entries, e.key)
	c.bytes -= e.size
}
