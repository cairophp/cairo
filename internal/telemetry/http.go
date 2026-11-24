package telemetry

import (
	"net"
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/cairophp/cairo/internal/middleware"
)

// HTTP wraps the router of an HTTP server: each request counts in the
// metrics and becomes a span, named by the route the router matched. It
// must wrap the router itself: the router names the route on the request it
// is given, and a handler in between that copies the request hides it.
// Requests that skip reports true for are left alone (the server's own
// health and metrics endpoints).
func (t *Telemetry) HTTP(router http.Handler, skip func(*http.Request) bool) http.Handler {
	if t == nil {
		return router
	}
	h := router
	if t.metrics != nil {
		h = t.metrics.measure(h, skip)
	}
	if t.tracer != nil {
		h = spans(h, t.tracer.Tracer(scope), skip)
	}
	return h
}

// scope names Cairo as the maker of its spans.
const scope = "github.com/cairophp/cairo"

// spans makes each request of the router a span, in the trace the request
// arrived in or in a new one, and gives PHP the span to continue: as the
// request's traceparent header, where PHP's own OpenTelemetry SDK looks for
// it, and as the Context value "traceparent".
//
// The span is made by hand on OpenTelemetry's SDK. Its instrumentation of
// net/http prepares a span's attributes, and those of metrics that nobody
// reads here, for the requests that are not recorded too: about 20 µs of
// CPU for each. Here such a request pays for its span's ids.
func spans(router http.Handler, tracer trace.Tracer, skip func(*http.Request) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if skip(r) {
			router.ServeHTTP(w, r)
			return
		}
		ctx := propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := tracer.Start(ctx, spanName(r), trace.WithSpanKind(trace.SpanKindServer))
		defer span.End()
		propagation.TraceContext{}.Inject(ctx, propagation.HeaderCarrier(r.Header))
		inner := r.WithContext(middleware.WithValue(ctx, "traceparent", r.Header.Get("Traceparent")))
		if !span.IsRecording() {
			router.ServeHTTP(w, inner)
			return
		}
		c := counted{ResponseWriter: w}
		router.ServeHTTP(&c, inner)
		describe(span, inner, c.status)
	})
}

// spanName is "GET /users/{id}" once the route is known, and the method
// until then and for a request that matched none.
func spanName(r *http.Request) string {
	m := method(r.Method)
	if m == "other" {
		m = "HTTP"
	}
	if r.Pattern == "" {
		return m
	}
	if r.Pattern[0] == '/' { // a route for every method
		return m + " " + r.Pattern
	}
	return r.Pattern
}

// describe gives a recorded span what is known once the router has run:
// its name by the route, and the attributes of OpenTelemetry's conventions
// for HTTP. The server's address is not among them: it would be the Host
// header, which any client can set. The path and the user agent are the
// client's too, and the tracer cuts them at its limit (startTracing).
func describe(span trace.Span, r *http.Request, status int) {
	if status == 0 {
		status = http.StatusOK // what net/http sends for a handler that wrote nothing
	}
	m := method(r.Method)
	if m == "other" {
		m = "_OTHER"
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	attrs := append(make([]attribute.KeyValue, 0, 9),
		semconv.HTTPRequestMethodKey.String(m),
		semconv.URLScheme(scheme),
		semconv.URLPath(r.URL.Path),
		semconv.NetworkProtocolVersion(protocol(r)),
		semconv.HTTPResponseStatusCode(status),
	)
	if p := route(r.Pattern); p != "" {
		attrs = append(attrs, semconv.HTTPRoute(p))
	}
	if ua := r.UserAgent(); ua != "" {
		attrs = append(attrs, semconv.UserAgentOriginal(ua))
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		attrs = append(attrs, semconv.ClientAddress(host))
	}
	if status >= 500 && status < 600 {
		attrs = append(attrs, semconv.ErrorTypeKey.String(statuses[status-100]))
		span.SetStatus(codes.Error, "")
	}
	span.SetName(spanName(r))
	span.SetAttributes(attrs...)
}

// protocol is the version of HTTP as the conventions spell it.
func protocol(r *http.Request) string {
	switch {
	case r.ProtoMajor == 3:
		return "3"
	case r.ProtoMajor == 2:
		return "2"
	case r.ProtoMinor == 0:
		return "1.0"
	}
	return "1.1"
}
