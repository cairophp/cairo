package test

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

// startFrameworks runs examples/frameworks, installing its Composer packages
// first. It skips when Composer is not available.
func startFrameworks(t *testing.T) string {
	t.Helper()
	dir := "../examples/frameworks"
	if _, err := os.Stat(dir + "/vendor/autoload.php"); err != nil {
		if _, err := exec.LookPath("composer"); err != nil {
			t.Skip("composer is not installed")
		}
		for _, args := range [][]string{
			{"install", "--no-interaction", "--no-progress"},
			{"install", "--no-interaction", "--no-progress", "--prefer-source"}, // no dist downloads
		} {
			cmd := exec.Command("composer", args...)
			cmd.Dir = dir
			if out, err := cmd.CombinedOutput(); err == nil {
				break
			} else {
				t.Logf("composer %v: %v\n%s", args, err, out)
			}
		}
		if _, err := os.Stat(dir + "/vendor/autoload.php"); err != nil {
			t.Skip("composer install failed (see above)")
		}
	}
	addr := freeAddr()
	cmd := exec.Command(cairoBin, "serve", dir+"/server.php", "--threads", "1", "--offload-threads", "1")
	cmd.Env = append(os.Environ(), "CAIRO_HTTP="+addr)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		_ = cmd.Wait()
	})
	if err := waitFor(addr, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	return "http://" + addr
}

func doJSON(t *testing.T, req *http.Request, wantStatus int) (map[string]any, http.Header) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s %s: status %d, want %d: %s", req.Method, req.URL, resp.StatusCode, wantStatus, raw)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return out, resp.Header
}

func TestPSR15Handler(t *testing.T) {
	base := startFrameworks(t)

	// A route with a parameter, a multipart body with a nested file field.
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("name", "Mohamed")
	_ = mw.WriteField("tags[]", "a")
	fw, _ := mw.CreateFormFile("docs[]", "one.txt")
	_, _ = fw.Write([]byte("first"))
	_ = mw.Close()
	req, _ := http.NewRequest("POST", base+"/psr/42?x=1&y[]=2", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Test", "hello")
	req.AddCookie(&http.Cookie{Name: "session", Value: "abc"})
	out, hdr := doJSON(t, req, 201)
	if got := hdr.Values("X-Handler"); !reflect.DeepEqual(got, []string{"psr15", "echo"}) {
		t.Errorf("X-Handler = %v", got)
	}
	checks := map[string]any{
		"kind":       "psr15",
		"method":     "POST",
		"protocol":   "1.1",
		"header":     "hello",
		"query":      map[string]any{"x": "1", "y": []any{"2"}},
		"cookies":    map[string]any{"session": "abc"},
		"parsed":     map[string]any{"name": "Mohamed", "tags": []any{"a"}},
		"files":      map[string]any{"docs": []any{map[string]any{"name": "one.txt", "size": float64(5), "body": "first"}}},
		"attributes": map[string]any{"id": "42"},
		"context":    true,
		"remote":     "127.0.0.1",
	}
	for k, want := range checks {
		if !reflect.DeepEqual(out[k], want) {
			t.Errorf("%s = %#v, want %#v", k, out[k], want)
		}
	}
	if uri, _ := out["uri"].(string); !strings.HasSuffix(uri, "/psr/42?x=1&y%5B%5D=2") || !strings.HasPrefix(uri, "http://127.0.0.1:") {
		t.Errorf("uri = %q", uri)
	}

	// The fallback: any method, any path no route matches.
	req, _ = http.NewRequest("DELETE", base+"/anything/else", strings.NewReader(`{"a":1}`))
	out, _ = doJSON(t, req, 201)
	if out["method"] != "DELETE" || out["body"] != `{"a":1}` || out["parsed"] != nil {
		t.Errorf("fallback: %v", out)
	}
	// Routes still win over the fallback.
	resp, err := http.Get(base + "/cairo")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "cairo route" {
		t.Errorf("/cairo = %q", b)
	}
}

func TestSymfonyKernel(t *testing.T) {
	base := startFrameworks(t)
	if resp, err := http.Get(base + "/missing-extensions"); err == nil {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(b) != "[]" {
			t.Skipf("this PHP build lacks extensions symfony/http-foundation needs: %s", b)
		}
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("title", "report")
	fw, _ := mw.CreateFormFile("doc", "r.txt")
	_, _ = fw.Write([]byte("contents"))
	_ = mw.Close()
	req, _ := http.NewRequest("POST", base+"/sf/7?page=2", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Test", "sf")
	req.AddCookie(&http.Cookie{Name: "session", Value: "xyz"})
	out, hdr := doJSON(t, req, 202)
	want := map[string]any{
		"kind": "symfony", "method": "POST", "path": "/sf/7",
		"query": map[string]any{"page": "2"}, "post": map[string]any{"title": "report"}, "json": nil,
		"cookie": "xyz", "header": "sf", "file": []any{"r.txt", true, "contents"}, "id": "7",
		"secure": false, "client_ip": "127.0.0.1",
	}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("symfony request:\n got  %v\n want %v", out, want)
	}
	if c := hdr.Get("Set-Cookie"); !strings.HasPrefix(c, "seen=yes") {
		t.Errorf("Set-Cookie = %q", c)
	}

	req, _ = http.NewRequest("POST", base+"/sf/8", strings.NewReader(`{"n":3}`))
	req.Header.Set("Content-Type", "application/json")
	out, _ = doJSON(t, req, 202)
	if !reflect.DeepEqual(out["json"], map[string]any{"n": float64(3)}) {
		t.Errorf("json body: %v", out["json"])
	}

	resp, err := http.Get(base + "/sf/stream")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "part 1\npart 2\n" {
		t.Errorf("streamed response = %q", b)
	}

	// terminate() ran after each response.
	resp, err = http.Get(base + "/sf/terminated")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	var terminated []string
	_ = json.Unmarshal(b, &terminated)
	if !reflect.DeepEqual(terminated, []string{"/sf/7", "/sf/8", "/sf/stream"}) {
		t.Errorf("terminated = %s", b)
	}
}
