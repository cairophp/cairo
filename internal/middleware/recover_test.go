package middleware

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestHTTPPanicBecomes500(t *testing.T) {
	var logs bytes.Buffer
	boom := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })
	h, err := HTTP(boom, nil, slog.New(slog.NewTextHandler(&logs, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Code != 500 {
		t.Fatalf("status %d, want 500", rec.Code)
	}
	if !strings.Contains(logs.String(), "panic=boom") || !strings.Contains(logs.String(), "recover_test.go") {
		t.Fatalf("log has no panic and stack: %s", logs.String())
	}
}

func TestHTTPAbortHandlerStillAborts(t *testing.T) {
	abort := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })
	h, _ := HTTP(abort, nil, slog.New(slog.DiscardHandler), nil)
	defer func() {
		if v := recover(); v != http.ErrAbortHandler {
			t.Fatalf("recovered %v, want http.ErrAbortHandler passed on", v)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
}

func TestGRPCPanicBecomesInternal(t *testing.T) {
	var logs bytes.Buffer
	ics, err := GRPC(nil, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil || len(ics) != 1 {
		t.Fatalf("interceptors: %d, %v", len(ics), err)
	}
	err = ics[0](nil, nil, &grpc.StreamServerInfo{FullMethod: "/svc/M"}, func(any, grpc.ServerStream) error { panic("boom") })
	if status.Code(err) != codes.Internal {
		t.Fatalf("err = %v, want Internal", err)
	}
	if !strings.Contains(logs.String(), "method=/svc/M") || !strings.Contains(logs.String(), "panic=boom") {
		t.Fatalf("log: %s", logs.String())
	}
}
