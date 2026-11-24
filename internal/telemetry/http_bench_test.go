package telemetry

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc/stats"
)

type discard struct{ h http.Header }

func (d *discard) Header() http.Header         { return d.h }
func (d *discard) Write(b []byte) (int, error) { return len(b), nil }
func (d *discard) WriteHeader(int)             {}

// What a request pays in Go for being measured and traced: the router
// alone, with metrics, with a span that is not recorded and one that is,
// and with both.
func BenchmarkHTTP(b *testing.B) {
	router := http.NewServeMux()
	router.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") })
	metrics, err := Start(context.Background(), Options{Metrics: true, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		b.Fatal(err)
	}
	recorded := sdktrace.NewTracerProvider(sdktrace.WithBatcher(noExport{}))
	dropped := sdktrace.NewTracerProvider(sdktrace.WithBatcher(noExport{}), sdktrace.WithSampler(sdktrace.NeverSample()))
	skip := func(*http.Request) bool { return false }
	for _, c := range []struct {
		name string
		tel  *Telemetry
	}{
		{"off", nil},
		{"metrics", metrics},
		{"tracing, not recorded", &Telemetry{tracer: dropped}},
		{"tracing", &Telemetry{tracer: recorded}},
		{"metrics and tracing", &Telemetry{metrics: metrics.metrics, tracer: recorded}},
	} {
		h := c.tel.HTTP(router, skip)
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				w := &discard{h: http.Header{}}
				for pb.Next() {
					req := httptest.NewRequest("GET", "http://localhost/users/7", nil)
					h.ServeHTTP(w, req)
				}
			})
		})
	}
}

// What a gRPC call pays for its span, which is OpenTelemetry's
// instrumentation of grpc-go: the events of one unary call.
func BenchmarkGRPCSpan(b *testing.B) {
	known := func(string) bool { return true }
	for name, sampler := range map[string]sdktrace.Sampler{"not recorded": sdktrace.NeverSample(), "recorded": sdktrace.AlwaysSample()} {
		tel := &Telemetry{tracer: sdktrace.NewTracerProvider(sdktrace.WithBatcher(noExport{}), sdktrace.WithSampler(sampler))}
		h := tel.statsHandler(known)
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				ctx := h.TagRPC(context.Background(), &stats.RPCTagInfo{FullMethodName: "/helloworld.Greeter/SayHello"})
				h.HandleRPC(ctx, &stats.Begin{})
				h.HandleRPC(ctx, &stats.InPayload{Length: 16})
				h.HandleRPC(ctx, &stats.OutPayload{Length: 16})
				h.HandleRPC(ctx, &stats.End{})
			}
		})
	}
}

type noExport struct{}

func (noExport) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error { return nil }
func (noExport) Shutdown(context.Context) error                             { return nil }
