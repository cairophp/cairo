package test

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

type openFDs struct {
	OpenFDs     int `json:"open_fds"`
	CurlVersion int `json:"curl_version"`
}

func cairoFDs(t *testing.T) openFDs {
	t.Helper()
	var out openFDs
	if err := json.Unmarshal([]byte(get(t, "/curl/open-fds")), &out); err != nil {
		t.Fatal(err)
	}
	if out.OpenFDs < 0 {
		t.Skip("no /proc/self/fd on this system")
	}
	return out
}

// With more than FD_SETSIZE (1024) descriptors open, curl's new sockets get
// numbers an fd_set cannot hold. Hooked curl must still wait for them:
// before the socket callback, each wait fell back to a 100 ms poll, so every
// transfer took 200 ms or more.
func TestHookedCurlPastFDSetSize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	// Idle client connections fill the server's lowest descriptors.
	const idle = 1200
	for i := 0; i < idle; i++ {
		c, err := net.Dial("tcp", httpAddr)
		if err != nil {
			t.Fatalf("connection %d: %v", i, err)
		}
		t.Cleanup(func() { c.Close() })
	}
	deadline := time.Now().Add(10 * time.Second)
	fds := cairoFDs(t)
	for fds.OpenFDs < idle && time.Now().Before(deadline) {
		fds = cairoFDs(t)
	}
	if fds.OpenFDs < idle {
		t.Fatalf("cairo holds %d descriptors, want more than %d", fds.OpenFDs, idle)
	}

	paths := map[string]string{
		"curl_exec":  "/curl?url=" + url.QueryEscape(srv.URL),
		"curl_multi": "/curl-multi?urls=" + url.QueryEscape(srv.URL+","+srv.URL),
	}
	for name, path := range paths {
		if name == "curl_multi" && fds.CurlVersion < 0x080800 {
			t.Logf("curl_multi: skipped, libcurl %x has no curl_multi_waitfds()", fds.CurlVersion)
			continue
		}
		for i := 0; i < 5; i++ {
			start := time.Now()
			get(t, path)
			if took := time.Since(start); took > 100*time.Millisecond {
				t.Fatalf("%s with %d descriptors open took %s; curl's sockets were not waited on", name, fds.OpenFDs, took)
			}
		}
	}
}

// async() tasks that fail while their exception holds a curl handle leave a
// reference cycle. Cairo collects it when the request ends, so the handles'
// descriptors (each hooked handle keeps a multi handle) are freed instead of
// piling up until PHP's collector runs.
func TestFailedTasksFreeCurlHandles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	const n = 50
	path := fmt.Sprintf("/curl/failed-tasks?n=%d&url=%s", n, url.QueryEscape(srv.URL))
	get(t, path)                        // warm up: the thread's idle fibers and connections
	time.Sleep(1100 * time.Millisecond) // collections run at most once a second
	before := cairoFDs(t).OpenFDs
	if got := get(t, path); got != fmt.Sprintf(`{"failed":%d}`, n) {
		t.Fatalf("got %s", got)
	}
	after := cairoFDs(t).OpenFDs
	t.Logf("descriptors before %d, after %d failed tasks: %d", before, n, after)
	if after-before >= n {
		t.Fatalf("%d descriptors more after %d failed tasks; their curl handles were not freed", after-before, n)
	}
}
