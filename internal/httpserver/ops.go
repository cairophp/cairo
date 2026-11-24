package httpserver

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/cairophp/cairo/internal/telemetry"
)

// The endpoints of an HTTP server that Go answers by itself, for the
// machines that run the process: $http->metrics() for Prometheus, and
// $http->health() for the probes of an orchestrator or a load balancer.
// No PHP runs for them, so they answer while every PHP thread is busy.

// validateOps checks the endpoints against each other and against the
// routes: taken holds the "METHOD /path" of every route.
func (d *Declaration) validateOps(taken map[string]bool) error {
	for _, p := range []string{d.MetricsPath, d.LivePath, d.ReadyPath} {
		if p == "" {
			continue
		}
		// The path goes to the router as a pattern, where braces and a
		// trailing slash mean more than themselves.
		if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "{} ") || (len(p) > 1 && strings.HasSuffix(p, "/")) {
			return fmt.Errorf("%q on %s is not a plain path that starts with /", p, d.Listen)
		}
		if taken["GET "+p] {
			return fmt.Errorf("route GET %s is declared twice on %s", p, d.Listen)
		}
		taken["GET "+p] = true
	}
	return nil
}

// isOps reports whether a request is for one of the endpoints: they are
// neither traced nor counted with the requests of the app.
func (d *Declaration) isOps(r *http.Request) bool {
	p := r.URL.Path
	return p != "" && (p == d.MetricsPath || p == d.LivePath || p == d.ReadyPath)
}

// handleOps adds the declared endpoints to the router.
func (d *Declaration) handleOps(mux *http.ServeMux, tel *telemetry.Telemetry) error {
	if d.MetricsPath != "" {
		scrape := tel.Scrape()
		if scrape == nil {
			return fmt.Errorf("metrics are not set up for %s", d.MetricsPath)
		}
		mux.Handle("GET "+d.MetricsPath, scrape)
	}
	if d.LivePath != "" {
		mux.HandleFunc("GET "+d.LivePath, func(w http.ResponseWriter, _ *http.Request) { health(w, true) })
	}
	if d.ReadyPath != "" {
		mux.HandleFunc("GET "+d.ReadyPath, func(w http.ResponseWriter, _ *http.Request) { health(w, tel.Ready()) })
	}
	return nil
}

func health(w http.ResponseWriter, ok bool) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if !ok {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("stopping\n"))
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}
