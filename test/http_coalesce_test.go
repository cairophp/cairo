package test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 100 requests on one PHP thread ask a 500 ms upstream for the same URL,
// half with one Authorization header and half with another: with coalesce,
// the upstream sees one call per header, and each caller gets the answer
// for its own header.
func TestHTTPClientCoalesce(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(500 * time.Millisecond)
		fmt.Fprint(w, "for "+r.Header.Get("Authorization"))
	}))
	defer srv.Close()

	start := time.Now()
	var wg sync.WaitGroup
	for i := range 100 {
		auth := []string{"alice", "bob"}[i%2]
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get("http://" + httpAddr + "/client/coalesce?" + url.Values{"url": {srv.URL}, "auth": {auth}}.Encode())
			if err != nil {
				t.Error(err)
				return
			}
			defer resp.Body.Close()
			if b, _ := io.ReadAll(resp.Body); string(b) != "for "+auth {
				t.Errorf("%s got %d %q", auth, resp.StatusCode, b)
			}
		}()
	}
	wg.Wait()
	took := time.Since(start)
	if n := calls.Load(); n != 2 {
		t.Errorf("upstream got %d calls, want 2 (one per Authorization header)", n)
	}
	if took > 2*time.Second {
		t.Errorf("took %s; coalesced callers should wait together", took)
	}
	t.Logf("100 coalesced GETs to a 500 ms upstream: %d upstream calls, %s", calls.Load(), took.Round(time.Millisecond))

	// Once the shared call is over, the next request makes a new one.
	if _, err := http.Get("http://" + httpAddr + "/client/coalesce?" + url.Values{"url": {srv.URL}, "auth": {"alice"}}.Encode()); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 3 {
		t.Errorf("upstream got %d calls after a later request, want 3", n)
	}
}
