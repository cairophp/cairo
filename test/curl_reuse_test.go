package test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// Hooked curl_exec() keeps the connection alive between calls on one handle:
// three calls open one connection, as they would outside Cairo. So do three
// handles used one after the other: a new handle takes over the connections
// of one that was closed on the same thread.
func TestHookedCurlReusesConnections(t *testing.T) {
	for _, path := range []string{"/curl/reuse", "/curl/reuse-across-handles"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "ok")
		}))
		var out struct {
			Connects []int `json:"connects"`
		}
		raw := get(t, path+"?url="+url.QueryEscape(srv.URL))
		srv.Close()
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatalf("%s: %s: %v", path, raw, err)
		}
		if fmt.Sprint(out.Connects) != "[1 0 0]" {
			t.Fatalf("%s: new connections per curl_exec() = %v, want [1 0 0]", path, out.Connects)
		}
	}
}
