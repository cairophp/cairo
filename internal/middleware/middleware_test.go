package middleware

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func mw(name string, opts any) Declaration {
	b, _ := json.Marshal(opts)
	return Declaration{Name: name, Options: b}
}

func build(t *testing.T, h http.Handler, decl ...Declaration) http.Handler {
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
	if _, err := HTTP(hello, []Declaration{{Name: "nope"}}, slog.Default()); err == nil {
		t.Fatal("unknown middleware accepted")
	}
	if _, err := GRPC([]Declaration{{Name: "max_body"}}, slog.Default()); err == nil {
		t.Fatal("max_body accepted for gRPC")
	}
	if _, err := HTTP(hello, []Declaration{mw("timeout", map[string]int{"seconds": 0})}, slog.Default()); err == nil {
		t.Fatal("a timeout of 0 seconds accepted")
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
	if _, err := HTTP(hello, []Declaration{mw("timeout", map[string]any{"seconds": 0})}, slog.Default()); err == nil {
		t.Fatal("timeout of 0 accepted")
	}
	if ics, err := GRPC([]Declaration{mw("timeout", map[string]any{"seconds": 1})}, slog.Default()); err != nil || len(ics) != 2 { // panic recovery, then timeout
		t.Fatalf("grpc timeout: %v", err)
	}
}
