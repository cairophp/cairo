package httpserver

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/cairophp/cairo/internal/telemetry"
)

// A server with nothing but the endpoints answers them without PHP: the
// probes follow the process's readiness, and the metrics leave them out.
func TestOpsEndpoints(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	tel, err := telemetry.Start(context.Background(), telemetry.Options{Metrics: true, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
	decl := Declaration{Listen: "127.0.0.1:0", MetricsPath: "/metrics", LivePath: "/healthz", ReadyPath: "/readyz"}
	s, err := New(decl, nil, log, tel)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve() }()
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	get := func(path string) (int, string) {
		t.Helper()
		resp, err := http.Get("http://" + s.Addr() + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	for _, path := range []string{"/healthz", "/readyz"} {
		if code, body := get(path); code != 200 || body != "ok\n" {
			t.Errorf("GET %s: %d %q", path, code, body)
		}
	}
	get("/nothing-here")
	tel.Drain()
	if code, _ := get("/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("GET /readyz while draining: %d, want 503", code)
	}
	if code, _ := get("/healthz"); code != 200 {
		t.Errorf("GET /healthz while draining: %d, want 200", code)
	}
	code, metrics := get("/metrics")
	if code != 200 || !strings.Contains(metrics, `cairo_http_request_duration_seconds_count{method="GET",route="",status="404"} 1`) {
		t.Errorf("GET /metrics: %d, want the 404 counted:\n%s", code, metrics)
	}
	if strings.Contains(metrics, "healthz") || strings.Contains(metrics, `status="503"`) {
		t.Errorf("the metrics count the probes:\n%s", metrics)
	}
}

// A metrics endpoint needs the process's telemetry.
func TestMetricsEndpointWithoutTelemetry(t *testing.T) {
	if _, err := New(Declaration{Listen: "127.0.0.1:0", MetricsPath: "/metrics"}, nil, slog.New(slog.DiscardHandler), nil); err == nil {
		t.Fatal("want an error")
	}
}
