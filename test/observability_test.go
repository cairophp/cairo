package test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

// observed is examples/observability as a test runs it: the app's HTTP and
// gRPC servers, the server with the metrics and the probes, and a collector
// that keeps the spans the process sends.
type observed struct {
	http, grpc, ops string
	cmd             *exec.Cmd
	exited          chan struct{}

	mu       sync.Mutex
	spans    []*tracepb.Span
	services map[string]bool // the service.name of each batch
}

func startObserved(t *testing.T, args ...string) *observed {
	t.Helper()
	o := &observed{http: freeAddr(), grpc: freeAddr(), ops: freeAddr(), exited: make(chan struct{}), services: map[string]bool{}}
	collector := httptest.NewServer(http.HandlerFunc(o.collect))
	t.Cleanup(collector.Close)

	o.cmd = exec.Command(cairoBin, append([]string{"serve", "../examples/observability/server.php", "--threads", "1", "--offload-threads", "1"}, args...)...)
	o.cmd.Env = append(os.Environ(), "CAIRO_HTTP="+o.http, "CAIRO_GRPC="+o.grpc, "CAIRO_OPS="+o.ops,
		"OTEL_EXPORTER_OTLP_ENDPOINT="+collector.URL, "OTEL_BSP_SCHEDULE_DELAY=50")
	o.cmd.Stdout, o.cmd.Stderr = os.Stderr, os.Stderr
	if err := o.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = o.cmd.Wait(); close(o.exited) }()
	t.Cleanup(func() {
		_ = o.cmd.Process.Signal(os.Interrupt)
		<-o.exited
	})
	for _, addr := range []string{o.http, o.grpc, o.ops} {
		if err := waitFor(addr, 10*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	return o
}

// collect is the collector's POST /v1/traces.
func (o *observed) collect(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	var req coltrace.ExportTraceServiceRequest
	if err != nil || r.URL.Path != "/v1/traces" || proto.Unmarshal(body, &req) != nil {
		http.Error(w, "not an OTLP trace export", http.StatusBadRequest)
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, rs := range req.ResourceSpans {
		for _, kv := range rs.GetResource().GetAttributes() {
			if kv.Key == "service.name" {
				o.services[kv.Value.GetStringValue()] = true
			}
		}
		for _, ss := range rs.ScopeSpans {
			o.spans = append(o.spans, ss.Spans...)
		}
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
}

// trace waits until the collector has n spans of the trace and returns them.
func (o *observed) trace(t *testing.T, traceID string, n int) []*tracepb.Span {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var got []*tracepb.Span
		o.mu.Lock()
		for _, s := range o.spans {
			if hex.EncodeToString(s.TraceId) == traceID {
				got = append(got, s)
			}
		}
		o.mu.Unlock()
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("the collector has %d spans of trace %s, want %d: %v", len(got), traceID, n, got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (o *observed) get(t *testing.T, addr, path string, header http.Header) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = header
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func attr(s *tracepb.Span, key string) string {
	for _, kv := range s.Attributes {
		if kv.Key == key {
			if v := kv.Value.GetStringValue(); v != "" {
				return v
			}
			return kv.Value.String()
		}
	}
	return ""
}

const (
	callerTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
	callerSpan  = "00f067aa0ba902b7"
)

// A request that arrives in a trace gets a server span under its caller's,
// named by its route, and PHP is given that span to continue.
func TestTracingContinuesTheCallersTrace(t *testing.T) {
	o := startObserved(t)
	code, body := o.get(t, o.http, "/orders/7", http.Header{"Traceparent": {"00-" + callerTrace + "-" + callerSpan + "-01"}})
	var seen struct {
		Order       int    `json:"order"`
		Traceparent string `json:"traceparent"`
		Header      string `json:"header"`
	}
	if err := json.Unmarshal([]byte(body), &seen); err != nil || code != 200 || seen.Order != 7 {
		t.Fatalf("status %d body %q: %v", code, body, err)
	}

	span := o.trace(t, callerTrace, 1)[0]
	if span.Name != "GET /orders/{id}" {
		t.Errorf("the span is named %q, want the route", span.Name)
	}
	if span.Kind != tracepb.Span_SPAN_KIND_SERVER || hex.EncodeToString(span.ParentSpanId) != callerSpan {
		t.Errorf("kind %v under %x, want a server span under %s", span.Kind, span.ParentSpanId, callerSpan)
	}
	if got := attr(span, "http.route"); got != "/orders/{id}" {
		t.Errorf("http.route = %q", got)
	}
	want := "00-" + callerTrace + "-" + hex.EncodeToString(span.SpanId) + "-01"
	if seen.Traceparent != want || seen.Header != want {
		t.Errorf("PHP saw value %q and header %q, want the server span %q", seen.Traceparent, seen.Header, want)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.services["orders"] || len(o.services) != 1 {
		t.Errorf("service names %v, want orders as $app->tracing() declared", o.services)
	}
}

// PHP calls a gRPC service through Cairo's client: the call is a span under
// the request that makes it, and the service's span is under the call.
func TestTracingFollowsAGRPCCallFromPHP(t *testing.T) {
	o := startObserved(t)
	code, body := o.get(t, o.http, "/greet?"+url.Values{"target": {o.grpc}}.Encode(), nil)
	traceID := body[strings.LastIndex(body, " ")+1:]
	if code != 200 || !strings.HasPrefix(body, "Hello Cairo, in trace ") || len(traceID) != 32 {
		t.Fatalf("status %d body %q", code, body)
	}
	// The handler answered with the trace it ran in: the one of the request.
	var request, call, served *tracepb.Span
	for _, s := range o.trace(t, traceID, 3) {
		switch {
		case s.Name == "GET /greet":
			request = s
		case s.Kind == tracepb.Span_SPAN_KIND_CLIENT:
			call = s
		case s.Kind == tracepb.Span_SPAN_KIND_SERVER:
			served = s
		}
	}
	if request == nil || call == nil || served == nil {
		t.Fatalf("want the request, the client call and the served call in trace %s: %v %v %v", traceID, request, call, served)
	}
	if call.Name != "helloworld.Greeter/SayHello" || served.Name != call.Name {
		t.Errorf("gRPC spans are named %q and %q", call.Name, served.Name)
	}
	if string(call.ParentSpanId) != string(request.SpanId) || string(served.ParentSpanId) != string(call.SpanId) {
		t.Errorf("want request > client call > served call, got parents %x and %x", call.ParentSpanId, served.ParentSpanId)
	}
}

// The metrics endpoint has the requests by route and status, the gRPC
// calls, the PHP threads, the Go runtime and the process. What a client can
// choose freely (its Host header, the name of a gRPC method) labels nothing,
// and the endpoints themselves are not counted.
func TestMetricsEndpoint(t *testing.T) {
	o := startObserved(t)
	for i := 0; i < 3; i++ {
		o.get(t, o.http, "/orders/"+string(rune('1'+i)), http.Header{"X-Any": {"1"}})
	}
	req, _ := http.NewRequest(http.MethodGet, "http://"+o.http+"/broken", nil)
	req.Host = "chosen-by-the-client.example"
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 500 {
		t.Fatalf("GET /broken: %v %v", resp, err)
	} else {
		resp.Body.Close()
	}
	o.get(t, o.http, "/greet?"+url.Values{"target": {o.grpc}}.Encode(), nil)
	conn, err := grpc.NewClient(o.grpc, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Invoke(ctx, "/chosen.ByTheClient/Method", &emptypb.Empty{}, &emptypb.Empty{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("an unknown method: %v", err)
	}
	o.get(t, o.ops, "/healthz", nil)

	code, metrics := o.get(t, o.ops, "/metrics", nil)
	if code != 200 {
		t.Fatalf("GET /metrics: %d", code)
	}
	for _, want := range []string{
		`cairo_http_request_duration_seconds_count{method="GET",route="/orders/{id}",status="200"} 3`,
		`cairo_http_request_duration_seconds_count{method="GET",route="/broken",status="500"} 1`,
		`cairo_http_request_duration_seconds_bucket{method="GET",route="/greet",status="200",le="10"} 1`,
		`cairo_grpc_call_duration_seconds_count{code="OK",method="helloworld.Greeter/SayHello"} 1`,
		`cairo_php_threads{pool="request"} 1`,
		`cairo_php_threads{pool="offload"} 1`,
		`cairo_php_threads_busy 0`,
		`cairo_php_requests_in_flight 0`,
		`cairo_php_thread_restarts_total 0`,
		`go_goroutines `,
		`process_cpu_seconds_total `,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("the metrics have no %s", want)
		}
	}
	for _, not := range []string{"chosen", "healthz", `"/metrics"`} {
		if strings.Contains(metrics, not) {
			t.Errorf("the metrics name %q", not)
		}
	}
	if t.Failed() {
		t.Log(metrics)
	}
	// The serving port has neither the numbers nor the probes.
	for _, path := range []string{"/metrics", "/healthz", "/readyz"} {
		if code, _ := o.get(t, o.http, path, nil); code != 404 {
			t.Errorf("GET %s on the app's port: %d, want 404", path, code)
		}
	}
}

// From the stop signal on, the readiness probe says no while the servers
// keep answering for --drain; then the process stops and has sent its spans.
func TestReadinessWhileDraining(t *testing.T) {
	o := startObserved(t, "--drain", "1s")
	for _, path := range []string{"/healthz", "/readyz"} {
		if code, body := o.get(t, o.ops, path, nil); code != 200 || body != "ok\n" {
			t.Fatalf("GET %s: %d %q", path, code, body)
		}
	}
	stopped := time.Now()
	if err := o.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(900 * time.Millisecond)
	for {
		code, _ := o.get(t, o.ops, "/readyz", nil)
		if code == 503 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET /readyz is still %d after the stop signal", code)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if code, _ := o.get(t, o.ops, "/healthz", nil); code != 200 {
		t.Errorf("GET /healthz while draining: %d", code)
	}
	code, _ := o.get(t, o.http, "/orders/1", http.Header{"Traceparent": {"00-" + callerTrace + "-" + callerSpan + "-01"}})
	if code != 200 {
		t.Errorf("a request while draining: %d", code)
	}
	select {
	case <-o.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the process did not stop")
	}
	if took := time.Since(stopped); took < 900*time.Millisecond {
		t.Errorf("stopped after %v, before the second of --drain was over", took)
	}
	o.trace(t, callerTrace, 1) // the last span left with the process
}
