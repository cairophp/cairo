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
// three calls open one connection, as they would outside Cairo.
func TestHookedCurlReusesConnections(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()
	var out struct {
		Connects []int `json:"connects"`
	}
	raw := get(t, "/curl/reuse?url="+url.QueryEscape(srv.URL))
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	if fmt.Sprint(out.Connects) != "[1 0 0]" {
		t.Fatalf("new connections per curl_exec() = %v, want [1 0 0]", out.Connects)
	}
}
