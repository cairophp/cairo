// Package spec holds the server declaration that server.php builds during the
// declare phase. PHP sends it to Go as JSON; Go validates it and serves it.
package spec

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Spec is everything server.php declared.
type Spec struct {
	Threads        int `json:"threads"`
	OffloadThreads int `json:"offload_threads"`
	// DetectBlocking is App::detectBlocking(): log a request that keeps its
	// PHP thread busy longer than this many seconds (0: off).
	DetectBlocking float64 `json:"detect_blocking"`
	// SlowRequests is App::slowRequests(): log a request that takes longer
	// than this many seconds, with where its time went (0: off).
	SlowRequests float64 `json:"slow_requests"`
	// HeavyRoutes is App::heavyRoutes(): warn about a route that ran PHP
	// for longer than this many seconds without yielding (0: off).
	HeavyRoutes float64 `json:"heavy_routes"`
	// MaxInFlight caps the requests each PHP request thread holds at once
	// (0: no cap). Requests over it wait in Go until one finishes.
	MaxInFlight int          `json:"max_in_flight"`
	HTTP        []HTTPServer `json:"http"`
	GRPC        []GRPCServer `json:"grpc"`
}

// HTTPServer is one `$app->http(':8080')` declaration.
type HTTPServer struct {
	Listen       string       `json:"listen"`
	ReadTimeout  float64      `json:"read_timeout"`
	WriteTimeout float64      `json:"write_timeout"`
	Routes       []Route      `json:"routes"`
	Static       []StaticPath `json:"static"`
	Middleware   []Middleware `json:"middleware"`
}

// Middleware is one `$server->use(Middleware::...)`: Go runs it before PHP.
type Middleware struct {
	Name    string          `json:"name"`
	Options json.RawMessage `json:"options"`
}

// Route maps an HTTP method and path pattern to a PHP handler id.
type Route struct {
	ID     int    `json:"id"`
	Method string `json:"method"`
	Path   string `json:"path"`
	// Cache is the route's ->cache(...), nil when it has none.
	Cache *RouteCache `json:"cache,omitempty"`
}

// RouteCache lets Go answer repeated GET requests to a route from memory.
type RouteCache struct {
	TTL                  float64  `json:"ttl"`
	StaleWhileRevalidate float64  `json:"stale_while_revalidate"`
	Vary                 []string `json:"vary"`
	// Tags name the route's responses for Cairo\Cache::purge().
	Tags []string `json:"tags"`
}

// StaticPath is served by Go directly, with no PHP involved.
type StaticPath struct {
	Prefix string `json:"prefix"`
	Dir    string `json:"dir"`
}

// GRPCServer is one `$app->grpc(':9000')` declaration.
type GRPCServer struct {
	Listen      string       `json:"listen"`
	Protos      []string     `json:"protos"`
	MaxDuration float64      `json:"max_duration"`
	Services    []Service    `json:"services"`
	Middleware  []Middleware `json:"middleware"`
}

// Service maps a fully qualified gRPC service name to a PHP handler id.
type Service struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// Parse decodes and validates a spec sent by PHP.
func Parse(b []byte) (*Spec, error) {
	var s Spec
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("decode server spec: %w", err)
	}
	return &s, s.Validate()
}

// Validate checks the spec before anything is served.
func (s *Spec) Validate() error {
	if len(s.HTTP) == 0 && len(s.GRPC) == 0 {
		return fmt.Errorf("server.php declared no servers: call $app->http(...) or $app->grpc(...)")
	}
	if s.Threads < 0 || s.OffloadThreads < 0 || s.DetectBlocking < 0 || s.SlowRequests < 0 || s.HeavyRoutes < 0 || s.MaxInFlight < 0 {
		return fmt.Errorf("threads must be 'auto' or a positive number")
	}
	listens := map[string]bool{}
	ids := map[int]bool{}
	claim := func(listen string) error {
		if listen == "" {
			return fmt.Errorf("a server has an empty listen address")
		}
		if listens[listen] {
			return fmt.Errorf("two servers listen on %s", listen)
		}
		listens[listen] = true
		return nil
	}
	for _, h := range s.HTTP {
		if err := claim(h.Listen); err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, r := range h.Routes {
			key := strings.ToUpper(r.Method) + " " + r.Path
			if seen[key] {
				return fmt.Errorf("route %s is declared twice on %s", key, h.Listen)
			}
			seen[key] = true
			if !strings.HasPrefix(r.Path, "/") {
				return fmt.Errorf("route path %q must start with /", r.Path)
			}
			if ids[r.ID] {
				return fmt.Errorf("duplicate handler id %d", r.ID)
			}
			ids[r.ID] = true
			if err := r.Cache.validate(); err != nil {
				return fmt.Errorf("route %s: %w", key, err)
			}
		}
	}
	for _, g := range s.GRPC {
		if err := claim(g.Listen); err != nil {
			return err
		}
		if len(g.Services) > 0 && len(g.Protos) == 0 {
			return fmt.Errorf("gRPC server %s has services but no ->proto(...) files", g.Listen)
		}
		for _, sv := range g.Services {
			if ids[sv.ID] {
				return fmt.Errorf("duplicate handler id %d", sv.ID)
			}
			ids[sv.ID] = true
		}
	}
	return nil
}

// maxCacheTag is the longest tag name, in bytes.
const maxCacheTag = 128

func (c *RouteCache) validate() error {
	if c == nil {
		return nil
	}
	if c.TTL <= 0 || c.StaleWhileRevalidate < 0 {
		return fmt.Errorf("cache ttl must be positive and staleWhileRevalidate not negative")
	}
	for _, h := range c.Vary {
		if !validHeaderName(h) {
			return fmt.Errorf("cache vary: %q is not a header name", h)
		}
	}
	for _, t := range c.Tags {
		if t == "" || len(t) > maxCacheTag {
			return fmt.Errorf("cache tags: a tag must have 1 to %d bytes", maxCacheTag)
		}
	}
	return nil
}

// validHeaderName reports whether h is an HTTP token (RFC 9110).
func validHeaderName(h string) bool {
	if h == "" {
		return false
	}
	for _, c := range []byte(h) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0) {
			return false
		}
	}
	return true
}
