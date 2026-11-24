package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/cairophp/cairo/internal/spec"
)

// spanLimits bounds what a span keeps. A span holds values that a client
// chose, such as the path: each is cut at 2048 bytes, unless the
// environment sets a length of its own (OTEL_SPAN_ATTRIBUTE_VALUE_LENGTH_LIMIT).
func spanLimits() sdktrace.SpanLimits {
	limits := sdktrace.NewSpanLimits()
	if limits.AttributeValueLengthLimit < 0 {
		limits.AttributeValueLengthLimit = 2048
	}
	return limits
}

// tracing is $app->tracing() as server.php declared it.
type tracing struct {
	On bool `json:"on"`
	// Service names the service in its traces; "" leaves it to
	// OTEL_SERVICE_NAME, then to "cairo".
	Service string `json:"service"`
	// Endpoint is the collector's OTLP/HTTP address, as
	// OTEL_EXPORTER_OTLP_ENDPOINT has it; "" leaves it to the environment.
	Endpoint string `json:"endpoint"`
	// Sample is the share of traces that start here to record, 0 to 1; nil
	// leaves it to OTEL_TRACES_SAMPLER, then to all of them.
	Sample *float64 `json:"sample"`
}

func declared(s *spec.Spec) (tracing, error) {
	var t tracing
	if s == nil {
		return t, nil
	}
	if err := s.Section("tracing", &t); err != nil {
		return t, err
	}
	if t.Sample != nil && (*t.Sample < 0 || *t.Sample > 1) {
		return t, errors.New("tracing: sample must be between 0 and 1")
	}
	if t.Endpoint != "" {
		if _, err := t.tracesURL(); err != nil {
			return t, err
		}
	}
	return t, nil
}

// resource describes the service in its spans: the declared name over
// OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES over "cairo".
func (t tracing) resource(ctx context.Context) (*resource.Resource, error) {
	last := []attribute.KeyValue{}
	if t.Service != "" {
		last = append(last, attribute.String("service.name", t.Service))
	}
	return resource.New(ctx,
		resource.WithAttributes(attribute.String("service.name", "cairo")),
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithAttributes(last...),
	)
}

// tracesURL is where spans go: the declared endpoint is the collector's
// base address, to which OTLP adds the signal's path.
func (t tracing) tracesURL() (string, error) {
	u, err := url.Parse(t.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("tracing: endpoint %q is not an http or https address", t.Endpoint)
	}
	return strings.TrimRight(t.Endpoint, "/") + "/v1/traces", nil
}

// startTracing sends spans in batches with OTLP over HTTP. The exporter and
// the batches take the rest of their settings from the standard OTEL_
// variables of the environment (headers, timeouts, certificates).
func (t *Telemetry) startTracing(ctx context.Context, decl tracing) error {
	res, err := decl.resource(ctx)
	if err != nil {
		return err
	}
	var to []otlptracehttp.Option
	if decl.Endpoint != "" {
		u, err := decl.tracesURL()
		if err != nil {
			return err
		}
		to = append(to, otlptracehttp.WithEndpointURL(u))
	}
	exporter, err := otlptracehttp.New(ctx, to...)
	if err != nil {
		return err
	}
	opts := []sdktrace.TracerProviderOption{sdktrace.WithBatcher(exporter), sdktrace.WithResource(res), sdktrace.WithRawSpanLimits(spanLimits())}
	if decl.Sample != nil {
		// A request that arrives in a trace follows that trace's decision.
		opts = append(opts, sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(*decl.Sample))))
	}
	tp := sdktrace.NewTracerProvider(opts...)
	t.tracer = tp
	t.flush = tp.Shutdown
	return nil
}
