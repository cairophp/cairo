package httpserver

import (
	"fmt"
	"strings"

	"github.com/cairophp/cairo/internal/mercurehub"
	"github.com/cairophp/cairo/internal/middleware"
	"github.com/cairophp/cairo/internal/servertls"
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
	PublicDir string `json:"public_dir"`
	// Scripts is $http->scripts(): the directory whose PHP scripts answer
	// what no file and no other route does, in classic mode ("" when not
	// set). Its route has Scripts set.
	Scripts string `json:"scripts"`
	// ScriptsFront is the front controller of Scripts, by its name: what
	// runs for a path that names no script.
	ScriptsFront string `json:"scripts_front"`
	// Sendfile is $http->sendfile(): the directories whose files an answer
	// may name for Go to send (X-Sendfile).
	Sendfile   []SentDir                `json:"sendfile"`
	Middleware []middleware.Declaration `json:"middleware"`
	// TLS is $http->tls() or autoTls(): the server speaks HTTPS, and
	// HTTP/2 with the clients that ask for it. Nil: plain HTTP.
	TLS *servertls.Declaration `json:"tls"`
	// H2C is $http->h2c(): HTTP/2 without TLS, for a proxy that speaks it.
	H2C bool `json:"h2c"`
	// RedirectHTTP is $http->redirectHttp(): a second, plain listener
	// that sends its requests to this server ("" when not set).
	RedirectHTTP string `json:"redirect_http"`
	// MetricsPath is $http->metrics(), LivePath and ReadyPath are
	// $http->health(): endpoints Go answers itself ("" when not set).
	MetricsPath string `json:"metrics"`
	LivePath    string `json:"live"`
	ReadyPath   string `json:"ready"`
	// Mercure is $http->mercure(): the Mercure hub, which answers at
	// mercurehub.Path before any route. Nil: no hub.
	Mercure *mercurehub.Declaration `json:"mercure"`
}

// Route maps an HTTP method and path pattern to a PHP handler id.
type Route struct {
	ID     int    `json:"id"`
	Method string `json:"method"`
	Path   string `json:"path"`
	// Cache is the route's ->cache(...), nil when it has none.
	Cache *RouteCache `json:"cache,omitempty"`
	// NoTimeout is the route's ->noTimeout(): the timeout middleware
	// leaves its requests alone.
	NoTimeout bool `json:"no_timeout,omitempty"`
	// StreamBody is the route's ->streamBody(): PHP reads the request's
	// body in pieces instead of getting it whole.
	StreamBody bool `json:"stream_body,omitempty"`
	// Scripts marks the route of $http->scripts(), which runs the script
	// that the request's path names (scripts.go).
	Scripts bool `json:"scripts,omitempty"`
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
		if decls[i].Scripts != "" && !s.Classic {
			return nil, fmt.Errorf("http %s: scripts() needs App::concurrency('classic'): a script defines its functions once per PHP request", decls[i].Listen)
		}
	}
	return decls, nil
}

func (d *Declaration) validate(claims *spec.Claims) error {
	if err := claims.Listen(d.Listen); err != nil {
		return err
	}
	if err := d.validateTLS(claims); err != nil {
		return fmt.Errorf("http %s: %w", d.Listen, err)
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
	if d.Mercure != nil {
		if err := d.validateMercure(); err != nil {
			return err
		}
	}
	return d.validateOps(seen)
}

// validateMercure refuses the routes and endpoints that the hub would hide.
func (d *Declaration) validateMercure() error {
	paths := []string{d.MetricsPath, d.LivePath, d.ReadyPath}
	for _, r := range d.Routes {
		paths = append(paths, r.Path)
	}
	for _, p := range paths {
		if mercurehub.Serves(p) {
			return fmt.Errorf("%s on %s is the Mercure hub's: mercure() answers there", p, d.Listen)
		}
	}
	return d.Mercure.Validate()
}

func (d *Declaration) validateTLS(claims *spec.Claims) error {
	if d.TLS == nil {
		if d.RedirectHTTP != "" {
			return fmt.Errorf("redirectHttp() needs tls() or autoTls(): there is no HTTPS to send requests to")
		}
		return nil
	}
	if d.H2C {
		return fmt.Errorf("h2c() is HTTP/2 without TLS; with tls() or autoTls() HTTP/2 is already on")
	}
	if d.RedirectHTTP != "" {
		if err := claims.Listen(d.RedirectHTTP); err != nil {
			return err
		}
	}
	return d.TLS.Validate()
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
