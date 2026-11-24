// Package telemetry is what a Cairo process reports of itself to the
// outside: metrics for Prometheus to scrape, traces sent with OTLP, and
// whether it is ready for requests. The metrics are Prometheus' Go library;
// the traces are OpenTelemetry's Go SDK, with its instrumentation of
// grpc-go. Both are set up from what server.php declared, and the servers
// ask it to wrap their handlers.
//
// What is not declared costs nothing: with no metrics endpoint and no
// tracing, the servers' handlers are not wrapped at all.
package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/cairophp/cairo/internal/spec"
)

// Options is what Start needs of the process.
type Options struct {
	// Spec is what server.php declared; its "tracing" section is
	// $app->tracing().
	Spec *spec.Spec
	// Metrics is true when a server declared a metrics endpoint.
	Metrics bool
	// Threads reads the state of the PHP threads for the metrics.
	Threads func() Threads
	Log     *slog.Logger
}

// Telemetry is the process's metrics, traces and readiness. A nil one
// reports nothing and is always ready.
type Telemetry struct {
	tracer   trace.TracerProvider // nil when tracing is off
	metrics  *metrics             // nil when there is no metrics endpoint
	draining atomic.Bool
	flush    func(context.Context) error // sends the spans still held and stops the exporter
}

// current is the process's telemetry for the code that no server hands it
// to: the gRPC clients PHP opens (GRPCClient).
var current atomic.Pointer[Telemetry]

// propagator reads and writes the trace of a request in its headers: W3C
// Trace Context and Baggage.
var propagator = propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})

// Start sets up what was declared. Call Shutdown when the servers have
// stopped, so the last spans are sent.
func Start(ctx context.Context, o Options) (*Telemetry, error) {
	decl, err := declared(o.Spec)
	if err != nil {
		return nil, err
	}
	t := &Telemetry{}
	if !decl.On && !o.Metrics {
		return t, nil
	}
	if o.Metrics {
		t.metrics = startMetrics(o.Threads)
	}
	if decl.On {
		// Export failures come from the SDK's goroutines, which have
		// nobody to return them to.
		otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) { o.Log.Warn("telemetry", "err", err) }))
		if err := t.startTracing(ctx, decl); err != nil {
			return nil, fmt.Errorf("tracing: %w", err)
		}
	}
	current.Store(t)
	return t, nil
}

// Shutdown sends the spans still held and stops their exporter, up to ctx.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	if t == nil {
		return nil
	}
	current.CompareAndSwap(t, nil)
	if t.flush == nil {
		return nil
	}
	return t.flush(ctx)
}

// Drain makes the process not ready: it is about to stop.
func (t *Telemetry) Drain() {
	if t != nil {
		t.draining.Store(true)
	}
}

// Ready reports whether the process takes requests. Its servers listen only
// once the PHP threads are up, so it is ready from then until Drain.
func (t *Telemetry) Ready() bool { return t == nil || !t.draining.Load() }

// Scrape is the handler of the metrics endpoint, nil when no server
// declared one.
func (t *Telemetry) Scrape() http.Handler {
	if t == nil || t.metrics == nil {
		return nil
	}
	return t.metrics.scrape
}

// Traceparent is the W3C traceparent of the span ctx is in, for PHP to
// continue the trace: "" when the request is not traced.
func Traceparent(ctx context.Context) string {
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return ""
	}
	c := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, c)
	return c["traceparent"]
}
