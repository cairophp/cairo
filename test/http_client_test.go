package test

import (
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func echoServer() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"method":    r.Method,
			"query":     r.URL.RawQuery,
			"default":   r.Header.Get("X-Default"),
			"override":  r.Header.Values("X-Override"),
			"multi":     r.Header.Values("X-Multi"),
			"type":      r.Header.Get("Content-Type"),
			"body":      string(body),
			"body64":    base64.StdEncoding.EncodeToString(body),
			"userAgent": r.UserAgent(),
		})
	})
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/echo", http.StatusFound)
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	})
	return mux
}

func TestHTTPClient(t *testing.T) {
	srv := httptest.NewServer(echoServer())
	defer srv.Close()
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "secure")
	}))
	defer tlsSrv.Close()
	cafile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(cafile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsSrv.Certificate().Raw}), 0o644); err != nil {
		t.Fatal(err)
	}
	raw := get(t, "/client/features?"+url.Values{"base": {srv.URL + "/"}, "tls": {tlsSrv.URL}, "cafile": {cafile}}.Encode())
	var out struct {
		JSON struct {
			Status int            `json:"status"`
			Reason string         `json:"reason"`
			Echo   map[string]any `json:"echo"`
			Type   string         `json:"type"`
			OK     bool           `json:"ok"`
		} `json:"json"`
		Form     map[string]any `json:"form"`
		Raw      string         `json:"raw"`
		Redirect struct {
			Status int    `json:"status"`
			URL    string `json:"url"`
		} `json:"redirect"`
		NoRedirect struct {
			Status   int    `json:"status"`
			Location string `json:"location"`
		} `json:"no_redirect"`
		NotFound      int      `json:"not_found"`
		Head          string   `json:"head"`
		Timeout       []string `json:"timeout"`
		Refused       string   `json:"refused"`
		TLSUnverified string   `json:"tls_unverified"`
		TLSCAFile     string   `json:"tls_cafile"`
		TLSInsecure   string   `json:"tls_insecure"`
		Offload       struct {
			Status int    `json:"status"`
			Body   string `json:"body"`
		} `json:"offload"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	fail := func(why string) { t.Helper(); t.Fatalf("%s: got %s", why, strings.TrimSpace(raw)) }
	e := out.JSON.Echo
	if out.JSON.Status != 200 || out.JSON.Reason != "OK" || !out.JSON.OK || out.JSON.Type != "application/json" {
		fail("json response")
	}
	if e["method"] != "POST" || e["query"] != "q=x%20y" || e["default"] != "d" || e["type"] != "application/json" || e["body"] != `{"a":1}` {
		fail("json request")
	}
	if fmt.Sprint(e["override"]) != "[new]" || fmt.Sprint(e["multi"]) != "[1 2]" {
		fail("headers")
	}
	if out.Form["method"] != "PUT" || out.Form["body"] != "name=Cairo&tags%5B0%5D=a&tags%5B1%5D=b" || out.Form["type"] != "application/x-www-form-urlencoded" {
		fail("form request (and a base_uri path without a leading slash)")
	}
	if out.Raw != "00ff726177" {
		fail("binary body")
	}
	if out.Redirect.Status != 200 || out.Redirect.URL != srv.URL+"/echo" {
		fail("following a redirect")
	}
	if out.NoRedirect.Status != 302 || out.NoRedirect.Location != "/echo" {
		fail("follow_redirects false")
	}
	if out.NotFound != 404 || out.Head != "" {
		fail("404 and HEAD")
	}
	if strings.Join(out.Timeout, " ") != `Cairo\Http\ClientTimeoutException timeout` || out.Refused != "connect" {
		fail("transport errors")
	}
	if out.TLSUnverified != "tls" || out.TLSCAFile != "secure" || out.TLSInsecure != "secure" {
		fail("TLS verification")
	}
	if out.Offload.Status != 200 || !strings.Contains(out.Offload.Body, `"method":"GET"`) {
		fail("the client outside a fiber (offload thread)")
	}
}

// 50 requests each fetch a 1s endpoint through Cairo\Http\Client; they
// overlap on one PHP thread.
func TestHTTPClientDoesNotBlockTheThread(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Second)
		fmt.Fprint(w, "slow-ok")
	}))
	defer srv.Close()
	const n = 50
	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get("http://" + httpAddr + "/client/get?url=" + url.QueryEscape(srv.URL))
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(b), `"body":"slow-ok"`) || !strings.Contains(string(b), `"status":200`) {
				errs <- fmt.Errorf("status %d body %q", resp.StatusCode, b)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	took := time.Since(start)
	t.Logf("%d Cairo\\Http\\Client requests to a 1s endpoint, on 1 PHP thread: %s", n, took.Round(time.Millisecond))
	if took > 3*time.Second {
		t.Fatalf("took %s; the client blocked the thread", took)
	}
}
