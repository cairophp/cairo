package middleware

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cairophp/cairo/internal/spec"
)

func mw(name string, opts any) spec.Middleware {
	b, _ := json.Marshal(opts)
	return spec.Middleware{Name: name, Options: b}
}

func build(t *testing.T, h http.Handler, decl ...spec.Middleware) http.Handler {
	t.Helper()
	out, err := HTTP(h, decl, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

var hello = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Seen-Id", r.Header.Get("X-Request-Id"))
	io.WriteString(w, strings.Repeat("hello ", 500))
})

func TestRequestIDIsKeptOrMade(t *testing.T) {
	h := build(t, hello, mw("request_id", nil))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	id := rec.Header().Get("X-Request-Id")
	if len(id) != 32 || rec.Header().Get("X-Seen-Id") != id {
		t.Fatalf("new id %q, handler saw %q", id, rec.Header().Get("X-Seen-Id"))
	}
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Request-Id", "abc-123")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("X-Request-Id") != "abc-123" {
		t.Fatalf("kept id: %q", rec.Header().Get("X-Request-Id"))
	}
	req.Header.Set("X-Request-Id", "bad id\n")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("X-Request-Id") == "bad id\n" {
		t.Fatal("an invalid id was echoed")
	}
}

func TestCORSAnswersPreflightInGo(t *testing.T) {
	called := false
	h := build(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }),
		mw("cors", CORSOptions{Origins: []string{"https://app.example"}, MaxAge: 600}))
	req := httptest.NewRequest("OPTIONS", "/x", nil)
	req.Header.Set("Origin", "https://app.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "Content-Type")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 204 || called || rec.Header().Get("Access-Control-Allow-Origin") != "https://app.example" ||
		rec.Header().Get("Access-Control-Allow-Headers") != "Content-Type" || rec.Header().Get("Access-Control-Max-Age") != "600" {
		t.Fatalf("preflight: %d %v called=%v", rec.Code, rec.Header(), called)
	}
	req = httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Access-Control-Allow-Origin") != "" || !called {
		t.Fatalf("other origin got %v", rec.Header())
	}
}

func TestRateLimitTokenBucket(t *testing.T) {
	l, err := newLimiter(RateOptions{PerSecond: 2, Burst: 3})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0)
	l.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if ok, _ := l.allow("a"); !ok {
			t.Fatalf("burst request %d refused", i)
		}
	}
	if ok, wait := l.allow("a"); ok || wait != 500*time.Millisecond {
		t.Fatalf("4th: ok=%v wait=%v", ok, wait)
	}
	if ok, _ := l.allow("b"); !ok {
		t.Fatal("another client shares a's bucket")
	}
	now = now.Add(500 * time.Millisecond)
	if ok, _ := l.allow("a"); !ok {
		t.Fatal("no token after refill")
	}

	h := build(t, hello, mw("rate_limit", RateOptions{PerSecond: 1, Burst: 1}))
	codes := []int{}
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
		codes = append(codes, rec.Code)
		if rec.Code == 429 && rec.Header().Get("Retry-After") != "1" {
			t.Fatalf("Retry-After %q", rec.Header().Get("Retry-After"))
		}
	}
	if codes[0] != 200 || codes[1] != 429 {
		t.Fatalf("codes %v", codes)
	}
}

func TestGzip(t *testing.T) {
	h := build(t, hello, mw("gzip", map[string]int{"min_bytes": 1024}))
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip, br")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("not gzipped: %v", rec.Header())
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(zr)
	if string(body) != strings.Repeat("hello ", 500) {
		t.Fatal("body changed")
	}

	small := build(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		io.WriteString(w, "tiny")
	}), mw("gzip", nil))
	rec = httptest.NewRecorder()
	small.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != "tiny" || rec.Code != 201 {
		t.Fatalf("small response: %d %v %q", rec.Code, rec.Header(), rec.Body.String())
	}

	req.Header.Set("Accept-Encoding", "gzip;q=0")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "" {
		t.Fatal("gzipped although q=0")
	}
}

func TestMaxBody(t *testing.T) {
	h := build(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			http.Error(w, "too big", 413)
		}
	}), mw("max_body", map[string]int{"bytes": 10}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/", strings.NewReader(strings.Repeat("x", 11))))
	if rec.Code != 413 {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestOrderAndUnknown(t *testing.T) {
	// request_id declared first runs first, so access_log (inner) sees the id.
	if _, err := HTTP(hello, []spec.Middleware{{Name: "nope"}}, slog.Default()); err == nil {
		t.Fatal("unknown middleware accepted")
	}
	if _, err := GRPC([]spec.Middleware{{Name: "gzip"}}, slog.Default()); err == nil {
		t.Fatal("gzip accepted for gRPC")
	}
	if _, err := HTTP(hello, []spec.Middleware{mw("rate_limit", RateOptions{PerSecond: 0})}, slog.Default()); err == nil {
		t.Fatal("per_second 0 accepted")
	}
}

func TestTimeoutSetsADeadline(t *testing.T) {
	var left time.Duration
	h := build(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dl, ok := r.Context().Deadline()
		if ok {
			left = time.Until(dl)
		}
	}), mw("timeout", map[string]any{"seconds": 0.5}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if left <= 0 || left > 500*time.Millisecond {
		t.Fatalf("deadline in %s, want within 0.5s", left)
	}
	if _, err := HTTP(hello, []spec.Middleware{mw("timeout", map[string]any{"seconds": 0})}, slog.Default()); err == nil {
		t.Fatal("timeout of 0 accepted")
	}
	if ics, err := GRPC([]spec.Middleware{mw("timeout", map[string]any{"seconds": 1})}, slog.Default()); err != nil || len(ics) != 2 { // panic recovery, then timeout
		t.Fatalf("grpc timeout: %v", err)
	}
}
