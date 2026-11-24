package telemetry

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cairophp/cairo/internal/spec"
)

func TestDeclaredTracing(t *testing.T) {
	cases := map[string]struct {
		tracing string
		ok      bool
	}{
		"off":               {`{"on":false,"service":"","endpoint":"","sample":null}`, true},
		"on":                {`{"on":true,"service":"shop","endpoint":"http://collector:4318","sample":0.25}`, true},
		"sample over 1":     {`{"on":true,"sample":1.5}`, false},
		"negative sample":   {`{"on":true,"sample":-0.1}`, false},
		"endpoint no host":  {`{"on":true,"endpoint":"collector:4318"}`, false},
		"endpoint not http": {`{"on":true,"endpoint":"ftp://collector"}`, false},
	}
	for name, c := range cases {
		s, err := spec.Parse([]byte(`{"extensions":{"tracing":` + c.tracing + `}}`))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := declared(s); (err == nil) != c.ok {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestTracesURL(t *testing.T) {
	for endpoint, want := range map[string]string{
		"http://localhost:4318":         "http://localhost:4318/v1/traces",
		"https://otlp.example.com/":     "https://otlp.example.com/v1/traces",
		"https://example.com/otlp/base": "https://example.com/otlp/base/v1/traces",
	} {
		if got, err := (tracing{Endpoint: endpoint}).tracesURL(); got != want || err != nil {
			t.Errorf("%s: %q, %v; want %q", endpoint, got, err, want)
		}
	}
}

func TestSpanName(t *testing.T) {
	cases := []struct{ method, pattern, want string }{
		{"GET", "GET /users/{id}", "GET /users/{id}"},
		{"POST", "/", "POST /"}, // a route for every method
		{"GET", "", "GET"},
		{"WHATEVER", "", "HTTP"},
		{"WHATEVER", "/", "HTTP /"},
	}
	for _, c := range cases {
		if got := spanName(&http.Request{Method: c.method, Pattern: c.pattern}); got != c.want {
			t.Errorf("%s with pattern %q: %q, want %q", c.method, c.pattern, got, c.want)
		}
	}
}

// Nothing declared: the servers' handlers are not wrapped at all.
func TestOffWrapsNothing(t *testing.T) {
	for name, tel := range map[string]*Telemetry{"nil": nil, "nothing declared": {}} {
		router := http.NewServeMux()
		if got := tel.HTTP(router, nil); got != http.Handler(router) {
			t.Errorf("%s: the router is wrapped", name)
		}
		if tel.GRPCServer(nil) != nil || tel.Scrape() != nil || !tel.Ready() {
			t.Errorf("%s: want no gRPC options, no metrics endpoint, and ready", name)
		}
	}
	if GRPCClient() != nil {
		t.Error("gRPC clients are followed with nothing declared")
	}
}

// The labels of the HTTP metrics are the method, the route and the status:
// a client that makes up methods, hosts and paths adds no series of its own.
func TestHTTPMetricsLabels(t *testing.T) {
	tel, err := Start(context.Background(), Options{
		Metrics: true, Log: slog.New(slog.DiscardHandler),
		Threads: func() Threads {
			return Threads{Threads: 4, Busy: 1, OffloadThreads: 2, InFlight: 3, Restarts: 5, Recycled: 6}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
	router := http.NewServeMux()
	router.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, r *http.Request) {})
	h := tel.HTTP(router, func(r *http.Request) bool { return r.URL.Path == "/skipped" })
	for _, c := range []struct{ method, host, path string }{
		{"GET", "a.example", "/users/1"},
		{"GET", "b.example:81", "/users/2"},
		{"MADEUP", "c.example", "/made/up"},
		{"GET", "d.example", "/skipped"},
	} {
		req := httptest.NewRequest(c.method, "http://"+c.host+c.path, nil)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	rec := httptest.NewRecorder()
	tel.Scrape().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body, _ := io.ReadAll(rec.Result().Body)
	metrics := string(body)
	for _, want := range []string{
		`cairo_http_request_duration_seconds_count{method="GET",route="/users/{id}",status="200"} 2`,
		`cairo_http_request_duration_seconds_count{method="other",route="",status="404"} 1`,
		`cairo_php_threads{pool="request"} 4`,
		`cairo_php_threads{pool="offload"} 2`,
		`cairo_php_threads_busy 1`,
		`cairo_php_requests_in_flight 3`,
		`cairo_php_thread_restarts_total 5`,
		`cairo_php_thread_recycles_total 6`,
		`go_goroutines `,
		`process_cpu_seconds_total `,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("no %s", want)
		}
	}
	for _, not := range []string{"example", "MADEUP", "/made/up", "skipped"} {
		if strings.Contains(metrics, not) {
			t.Errorf("the metrics name %q", not)
		}
	}
	if t.Failed() {
		t.Log(metrics)
	}
}
