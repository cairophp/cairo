package test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// PHP calls this server's own Greeter: through the generated client, the
// generic Cairo\Grpc\Client with a .proto file, and from an offload thread.
func TestGRPCClient(t *testing.T) {
	raw := get(t, "/grpc-client/hello?"+url.Values{"target": {grpcAddr}, "name": {"Cairo"}}.Encode())
	var out struct {
		Typed          string          `json:"typed"`
		Generic        string          `json:"generic"`
		HasContentType bool            `json:"has_content_type"`
		Errors         map[string]bool `json:"errors"`
		Offload        string          `json:"offload"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	if out.Typed != "Hello Cairo" || out.Generic != "Hello array" || !out.HasContentType || out.Offload != "Hello offload" {
		t.Fatalf("got %s", strings.TrimSpace(raw))
	}
	for _, k := range []string{"deadline", "unimplemented", "unavailable"} {
		if !out.Errors[k] {
			t.Fatalf("%s: got %s", k, strings.TrimSpace(raw))
		}
	}
}

// 50 HTTP requests each make a 1s gRPC call to a handler on the same single
// PHP thread: the calling fibers and the handlers all overlap.
func TestGRPCClientDoesNotBlockTheThread(t *testing.T) {
	const n = 50
	q := url.Values{"target": {grpcAddr}}.Encode()
	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get("http://" + httpAddr + "/grpc-client/slow?" + q)
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(b), `"message":"Hello slow"`) {
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
	t.Logf("%d gRPC client calls to a 1s handler, on 1 PHP thread: %s", n, took.Round(time.Millisecond))
	if took > 3*time.Second {
		t.Fatalf("took %s; the gRPC client blocked the thread", took)
	}
}
