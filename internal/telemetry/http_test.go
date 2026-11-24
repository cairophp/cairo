package telemetry

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/cairophp/cairo/internal/middleware"
)

// traced serves requests through a router whose spans rec keeps, and
// returns what the handler saw of its span for each.
func traced(t *testing.T, sampler sdktrace.Sampler, requests ...*http.Request) (rec *tracetest.SpanRecorder, seen []string) {
	t.Helper()
	rec = tracetest.NewSpanRecorder()
	tel := &Telemetry{tracer: sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(rec), sdktrace.WithSampler(sampler), sdktrace.WithRawSpanLimits(spanLimits()))}
	router := http.NewServeMux()
	router.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, r *http.Request) {
		value, _ := middleware.Values(r.Context())["traceparent"].(string)
		seen = append(seen, value+" "+r.Header.Get("Traceparent"))
		if r.PathValue("id") == "0" {
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	h := tel.HTTP(router, func(r *http.Request) bool { return r.URL.Path == "/skipped" })
	for _, r := range requests {
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	return rec, seen
}

func attrs(s sdktrace.ReadOnlySpan) map[string]string {
	m := map[string]string{}
	for _, kv := range s.Attributes() {
		m[string(kv.Key)] = kv.Value.Emit()
	}
	return m
}

// A recorded span has the route as its name and the conventions'
// attributes, and nothing of it is the client's to choose but the path and
// the user agent, which are cut.
func TestHTTPSpan(t *testing.T) {
	long := httptest.NewRequest("MADEUP", "http://c.example/"+strings.Repeat("a", 5000), nil)
	long.Header.Set("User-Agent", strings.Repeat("u", 5000))
	rec, _ := traced(t, sdktrace.AlwaysSample(),
		httptest.NewRequest("GET", "http://a.example/users/7", nil),
		httptest.NewRequest("GET", "http://b.example/users/0", nil),
		long,
		httptest.NewRequest("GET", "http://d.example/skipped", nil),
	)
	spans := rec.Ended()
	if len(spans) != 3 {
		t.Fatalf("%d spans, want 3: the skipped request has none", len(spans))
	}
	ok, failed, unknown := spans[0], spans[1], spans[2]
	want := map[string]string{
		"http.request.method": "GET", "http.route": "/users/{id}", "http.response.status_code": "200",
		"url.path": "/users/7", "url.scheme": "http", "network.protocol.version": "1.1", "client.address": "192.0.2.1",
	}
	if got := attrs(ok); ok.Name() != "GET /users/{id}" || ok.Status().Code != codes.Unset || len(got) != len(want) {
		t.Errorf("span %q, status %v, attributes %v", ok.Name(), ok.Status(), got)
	}
	for k, v := range want {
		if got := attrs(ok)[k]; got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if got := attrs(failed); failed.Status().Code != codes.Error || got["http.response.status_code"] != "500" || got["error.type"] != "500" {
		t.Errorf("a 500: status %v, attributes %v", failed.Status(), got)
	}
	got := attrs(unknown)
	if unknown.Name() != "HTTP" || got["http.request.method"] != "_OTHER" || got["http.response.status_code"] != "404" || got["http.route"] != "" {
		t.Errorf("an unknown method and path: span %q, attributes %v", unknown.Name(), got)
	}
	if len(got["url.path"]) != 2048 || len(got["user_agent.original"]) != 2048 {
		t.Errorf("path of %d bytes and user agent of %d, want both cut at 2048", len(got["url.path"]), len(got["user_agent.original"]))
	}
	for _, s := range spans {
		for k, v := range attrs(s) {
			if strings.Contains(v, "example") || strings.Contains(v, "MADEUP") {
				t.Errorf("%s = %q is the client's Host or method", k, v)
			}
		}
	}
}

// PHP is given the span of its request to continue, also when the request
// is not recorded: what it calls must learn that the trace is not sampled.
func TestHTTPSpanReachesTheHandler(t *testing.T) {
	const trace, parent = "0af7651916cd43dd8448eb211c80319c", "b7ad6b7169203331"
	in := httptest.NewRequest("GET", "/users/7", nil)
	in.Header.Set("Traceparent", "00-"+trace+"-"+parent+"-01")
	rec, seen := traced(t, sdktrace.ParentBased(sdktrace.NeverSample()), in, httptest.NewRequest("GET", "/users/7", nil))
	spans := rec.Ended()
	if len(spans) != 1 || spans[0].Parent().SpanID().String() != parent {
		t.Fatalf("%d spans recorded, want the one whose caller was sampled, under it", len(spans))
	}
	if want := "00-" + trace + "-" + spans[0].SpanContext().SpanID().String() + "-01"; seen[0] != want+" "+want {
		t.Errorf("in the caller's trace the handler saw %q, want %q as value and header", seen[0], want)
	}
	value, header, _ := strings.Cut(seen[1], " ")
	if len(value) != 55 || !strings.HasSuffix(value, "-00") || value != header {
		t.Errorf("in a trace that is not sampled the handler saw %q and %q, want one traceparent that ends in -00", value, header)
	}
}
