package httpserver

import (
	"fmt"
	"strings"

	"github.com/cairophp/cairo/internal/middleware"
	"github.com/cairophp/cairo/internal/spec"
)

// Declaration is one `$app->http(':8080')` declaration.
type Declaration struct {
	Listen       string       `json:"listen"`
	ReadTimeout  float64      `json:"read_timeout"`
	WriteTimeout float64      `json:"write_timeout"`
	Routes       []Route      `json:"routes"`
	Static       []StaticPath `json:"static"`
	// PublicDir is $http->publicDir(): files served at their own paths,
	// with everything else going to the routes ("" when not set).
	PublicDir  string                   `json:"public_dir"`
	Middleware []middleware.Declaration `json:"middleware"`
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

// Declared reads the HTTP servers out of what server.php declared and checks
// them before anything is served. The listen addresses and handler ids they
// take go into claims.
func Declared(s *spec.Spec, claims *spec.Claims) ([]Declaration, error) {
	var decls []Declaration
	if err := s.Section("http", &decls); err != nil {
		return nil, err
	}
	for i := range decls {
		if err := decls[i].validate(claims); err != nil {
			return nil, err
		}
	}
	return decls, nil
}

func (d *Declaration) validate(claims *spec.Claims) error {
	if err := claims.Listen(d.Listen); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, r := range d.Routes {
		key := strings.ToUpper(r.Method) + " " + r.Path
		if seen[key] {
			return fmt.Errorf("route %s is declared twice on %s", key, d.Listen)
		}
		seen[key] = true
		if !strings.HasPrefix(r.Path, "/") {
			return fmt.Errorf("route path %q must start with /", r.Path)
		}
		if err := claims.Handler(r.ID); err != nil {
			return err
		}
		if err := r.Cache.validate(); err != nil {
			return fmt.Errorf("route %s: %w", key, err)
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
