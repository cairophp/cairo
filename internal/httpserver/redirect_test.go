package httpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestToHTTPS(t *testing.T) {
	for _, c := range []struct {
		port, host, target string
		want               string // the Location, "" for 400
	}{
		{"443", "example.com", "/", "https://example.com/"},
		{"443", "example.com:80", "/a/b?x=1&y=%2F", "https://example.com/a/b?x=1&y=%2F"},
		{"8443", "example.com:8080", "/a", "https://example.com:8443/a"},
		{"8443", "example.com", "/a", "https://example.com:8443/a"},
		{"443", "[::1]:80", "/", "https://[::1]/"},
		{"8443", "[::1]", "/", "https://[::1]:8443/"},
		{"443", "example.com", "//evil.test/x", "https://example.com//evil.test/x"},
		{"443", "", "/", ""},
	} {
		req := httptest.NewRequest(http.MethodPost, c.target, nil)
		req.Host = c.host
		rec := httptest.NewRecorder()
		toHTTPS(c.port).ServeHTTP(rec, req)
		if c.want == "" {
			if rec.Code != http.StatusBadRequest {
				t.Errorf("host %q: status %d, want 400", c.host, rec.Code)
			}
			continue
		}
		if got := rec.Header().Get("Location"); rec.Code != http.StatusPermanentRedirect || got != c.want {
			t.Errorf("host %q, %s: %d to %q, want 308 to %q", c.host, c.target, rec.Code, got, c.want)
		}
	}
}
