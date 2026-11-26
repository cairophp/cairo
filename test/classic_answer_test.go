package test

import (
	"io"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

// fastcgi_finish_request() answers the client at once, and the script runs
// on after it, on its thread.
func TestClassicFinishRequest(t *testing.T) {
	eachClassic(t, 1, func(t *testing.T, base string) {
		start := time.Now()
		resp, body := classicGet(t, http.DefaultClient, base+"/finish.php")
		if body != "answered\n" || resp.ContentLength != 9 || time.Since(start) > 500*time.Millisecond {
			t.Errorf("finish.php: %q, length %d, after %v", body, resp.ContentLength, time.Since(start))
		}
		// The one thread runs the script to its end before the next request.
		if _, body := classicGet(t, http.DefaultClient, base+"/state.php"); time.Since(start) < 700*time.Millisecond || !strings.Contains(body, `"hits":1`) {
			t.Errorf("next request after %v: %q", time.Since(start), body)
		}
	})
}

// headers_send(103) sends Early Hints before the page, and the credentials
// of Basic authentication are the script's, as under PHP-FPM.
func TestClassicEarlyHintsAndAuth(t *testing.T) {
	eachClassic(t, 1, func(t *testing.T, base string) {
		var hint string
		trace := &httptrace.ClientTrace{Got1xxResponse: func(code int, h textproto.MIMEHeader) error {
			if code == http.StatusEarlyHints {
				hint = h.Get("Link")
			}
			return nil
		}}
		req, _ := http.NewRequest(http.MethodGet, base+"/hints.php", nil)
		req.SetBasicAuth("ada", "secret")
		resp, err := http.DefaultClient.Do(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if hint != "</style.css>; rel=preload; as=style" || resp.StatusCode != 200 || string(body) != "page user=ada\n" {
			t.Errorf("hints.php: hint %q, then %d %q", hint, resp.StatusCode, body)
		}
	})
}
