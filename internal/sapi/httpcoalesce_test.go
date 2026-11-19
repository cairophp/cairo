package sapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHTTPFlightKey(t *testing.T) {
	base := httpClientRequest{Method: "GET", URL: "https://api.example/x", Headers: map[string][]string{"Authorization": {"a"}}, Timeout: 30, Redirects: 5, TLS: httpClientTLS{Verify: true}}
	same := base
	same.Headers = map[string][]string{"authorization": {"a"}}
	if httpFlightKey(base) != httpFlightKey(same) {
		t.Error("the same request with a lower-case header name got another key")
	}
	for name, change := range map[string]func(*httpClientRequest){
		"auth":      func(r *httpClientRequest) { r.Headers = map[string][]string{"Authorization": {"b"}} },
		"no header": func(r *httpClientRequest) { r.Headers = nil },
		"two values": func(r *httpClientRequest) {
			r.Headers = map[string][]string{"Authorization": {"a", ""}}
		},
		"cookie":   func(r *httpClientRequest) { r.Headers = map[string][]string{"Authorization": {"a"}, "Cookie": {"s=1"}} },
		"url":      func(r *httpClientRequest) { r.URL += "?y" },
		"method":   func(r *httpClientRequest) { r.Method = "HEAD" },
		"timeout":  func(r *httpClientRequest) { r.Timeout = 5 },
		"verify":   func(r *httpClientRequest) { r.TLS.Verify = false },
		"cert":     func(r *httpClientRequest) { r.TLS.Cert = "/c.pem" },
		"redirect": func(r *httpClientRequest) { r.Redirects = 0 },
	} {
		r := base
		change(&r)
		if httpFlightKey(r) == httpFlightKey(base) {
			t.Errorf("%s: a different request got the same key", name)
		}
	}
	if httpCanCoalesce(httpClientRequest{Method: "POST"}) || httpCanCoalesce(httpClientRequest{Method: "GET", Body: "eA=="}) {
		t.Error("only GET and HEAD without a body may coalesce")
	}
}

// A caller that leaves does not fail the others; when the last one leaves,
// the upstream call is cancelled.
func TestHTTPCoalescedCallersLeave(t *testing.T) {
	cancelled := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hold" { // answers only when cancelled
			<-r.Context().Done()
			close(cancelled)
			return
		}
		<-release
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()
	req := httpClientRequest{Method: "GET", URL: srv.URL, Coalesce: true}

	stay := make(chan map[string]any)
	go func() { stay <- httpCoalesced(context.Background(), req) }()
	leaver, leave := context.WithCancel(context.Background())
	left := make(chan map[string]any)
	go func() { left <- httpCoalesced(leaver, req) }()
	waitWaiters(t, req, 2)
	leave()
	if res := <-left; res["ok"] != false {
		t.Fatalf("the caller that left got %v", res)
	}
	close(release)
	if res := <-stay; res["ok"] != true || res["status"] != 200 {
		t.Fatalf("the caller that stayed got %v", res)
	}

	held := httpClientRequest{Method: "GET", URL: srv.URL + "/hold", Coalesce: true}
	gone, goAway := context.WithCancel(context.Background())
	done := make(chan map[string]any)
	go func() { done <- httpCoalesced(gone, held) }()
	waitWaiters(t, held, 1)
	goAway()
	<-done
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("the upstream call went on after every caller left")
	}
}

func waitWaiters(t *testing.T, req httpClientRequest, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		httpFlightsMu.Lock()
		f := httpFlights[httpFlightKey(req)]
		ok := f != nil && f.waiters == n
		httpFlightsMu.Unlock()
		if ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("never saw %d waiters", n)
}
