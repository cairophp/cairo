package test

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// startStreaming runs a server of examples/streaming on one PHP thread and
// returns its base URL.
func startStreaming(t *testing.T, script string, env ...string) string {
	t.Helper()
	base, _ := runStreaming(t, script, env...)
	return base
}

// runStreaming is startStreaming for a test that stops the server itself:
// stop asks it to, as a deploy does, and returns once it has.
func runStreaming(t *testing.T, script string, env ...string) (base string, stop func()) {
	t.Helper()
	addr := freeAddr()
	logs := &syncBuffer{}
	cmd := exec.Command(cairoBin, "serve", "../examples/streaming/"+script, "--threads", "1", "--offload-threads", "1")
	cmd.Env = append(append(os.Environ(), "CAIRO_HTTP="+addr), env...)
	cmd.Stdout, cmd.Stderr = logs, logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stop = sync.OnceFunc(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		_ = cmd.Wait()
	})
	t.Cleanup(func() {
		http.DefaultClient.CloseIdleConnections()
		stop()
		if t.Failed() {
			t.Logf("server log:\n%s", logs.String())
		}
	})
	if err := waitFor(addr, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	return "http://" + addr, stop
}

// fetchBody is the body of a GET that must succeed.
func fetchBody(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Error(err)
		return ""
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || err != nil {
		t.Errorf("GET %s: status %d: %s (%v)", url, resp.StatusCode, b, err)
	}
	return string(b)
}

// seen polls /seen until the server has noted key, and returns the note.
func seen(t *testing.T, base, key string) string {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		var log map[string]string
		_ = json.Unmarshal([]byte(fetchBody(t, base+"/seen")), &log)
		if v, ok := log[key]; ok {
			return v
		}
	}
	t.Fatalf("the server never noted %q", key)
	return ""
}

// The client has the head and the first piece while the handler still
// waits, and fifty such streams share one PHP thread: each is a fiber.
func TestStreamedAnswer(t *testing.T) {
	base := startStreaming(t, "server.php")

	start := time.Now()
	resp, err := http.Get(base + "/stream?wait=0.5")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)
	first, _ := r.ReadString('\n')
	early := time.Since(start)
	rest, err := io.ReadAll(r)
	if first != "first\n" || string(rest) != "second\n" || err != nil {
		t.Fatalf("body = %q then %q (%v)", first, rest, err)
	}
	if early > 400*time.Millisecond || time.Since(start) < 500*time.Millisecond {
		t.Errorf("the first piece came after %v and the last after %v: not while the handler waited", early, time.Since(start))
	}
	if resp.ContentLength != -1 || resp.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Errorf("length %d, type %q", resp.ContentLength, resp.Header.Get("Content-Type"))
	}

	const n = 50
	var wg sync.WaitGroup
	start = time.Now()
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if body := fetchBody(t, base+"/stream?wait=1"); body != "first\nsecond\n" {
				t.Errorf("body = %q", body)
			}
		}()
	}
	wg.Wait()
	if took := time.Since(start); took > 2500*time.Millisecond {
		t.Errorf("%d streams that each wait 1s took %v on one PHP thread", n, took)
	}
}

// More than Go queues for a client arrives whole and in order, and a client
// that reads slowly holds its handler back instead of losing its stream to
// the write timeout, which is for a client that does not read at all.
func TestStreamAndItsReader(t *testing.T) {
	base := startStreaming(t, "server.php", "CAIRO_WRITE_TIMEOUT=0.3")
	line := strings.Repeat("x", 1023) + "\n"

	resp, err := http.Get(base + "/large?kb=3072")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// 64 KiB every 20 ms: three megabytes take a second, three write timeouts.
	got, piece := 0, make([]byte, 64<<10)
	for tick := time.NewTicker(20 * time.Millisecond); ; <-tick.C {
		n, err := io.ReadFull(resp.Body, piece)
		if !bytes.Equal(piece[:n], []byte(strings.Repeat(line, 64))[:n]) {
			t.Fatalf("the stream is damaged after %d bytes", got)
		}
		if got += n; err != nil {
			tick.Stop()
			break
		}
	}
	if got != 3072<<10 {
		t.Errorf("a slow reader got %d bytes of %d", got, 3072<<10)
	}

	// A client that asks and never reads: its handler is told.
	conn, err := net.Dial("tcp", strings.TrimPrefix(base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET /large?kb=262144 HTTP/1.1\r\nHost: test\r\n\r\n")
	if note := seen(t, base, "large"); note != "the client stopped reading" {
		t.Errorf("note = %q", note)
	}
}

func TestServerSentEvents(t *testing.T) {
	base := startStreaming(t, "server.php", "CAIRO_TIMEOUT=0.2")

	// The route has noTimeout(): its events outlast the server's limit.
	req, _ := http.NewRequest("GET", base+"/events?count=6", nil)
	req.Header.Set("Last-Event-ID", "5")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	want := "retry: 2000\n\n"
	for i := 6; i <= 11; i++ {
		want += fmt.Sprintf("event: tick\nid: %d\ndata: {\"tick\":%d}\n\n", i, i)
	}
	want += "data: two\ndata: lines\n\n"
	if string(body) != want || err != nil {
		t.Errorf("events = %q (%v), want %q", body, err, want)
	}
	if h := resp.Header; h.Get("Content-Type") != "text/event-stream" || h.Get("Cache-Control") != "no-cache" {
		t.Errorf("headers = %v", h)
	}

	// A stream on a route without it is cut at the limit: the head is with
	// the client, so no status can say it.
	resp, err = http.Get(base + "/stream?wait=1")
	if err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "first\n" || err == nil {
		t.Errorf("a stream past the time limit: %q (%v), want the first piece and an error", body, err)
	}

	// A stream ends when its client leaves.
	resp, err = http.Get(base + "/forever")
	if err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(resp.Body).ReadString('\n')
	resp.Body.Close()
	if line != ": still here\n" {
		t.Errorf("comment = %q", line)
	}
	if note := seen(t, base, "forever"); note != "ended" {
		t.Errorf("note = %q", note)
	}
}

// A stream has no end for a stopping server to wait for. Its handler is
// refused its next piece, as when its client leaves, so the server stops at
// once instead of at the end of its grace time (30 s), and the client gets
// an answer that ends as one should.
func TestStreamEndsWhenTheServerStops(t *testing.T) {
	base, stop := runStreaming(t, "server.php")
	resp, err := http.Get(base + "/forever")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := bufio.NewReader(resp.Body)
	if line, _ := body.ReadString('\n'); line != ": still here\n" {
		t.Fatalf("comment = %q", line)
	}

	start := time.Now()
	stop()
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the server took %s to stop with a stream open", took)
	}
	if _, err := io.ReadAll(body); err != nil {
		t.Errorf("the stream of a stopped server: %v, want a clean end", err)
	}
}

// A handler that fails after its answer began cannot change the status: the
// client sees an answer that does not end as one should.
func TestStreamThatFails(t *testing.T) {
	base := startStreaming(t, "server.php")
	resp, err := http.Get(base + "/fails")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "so far so good\n" || err == nil {
		t.Errorf("status %d, body %q, err %v: want what was sent and an error", resp.StatusCode, body, err)
	}
	if body := fetchBody(t, base+"/stream?wait=0"); body != "first\nsecond\n" {
		t.Errorf("the next stream = %q", body)
	}
}

// hinted makes a request and returns the Link header of its 103 response,
// with the final response.
func hinted(t *testing.T, url string) (string, *http.Response, string) {
	t.Helper()
	var hint string
	trace := &httptrace.ClientTrace{Got1xxResponse: func(code int, h textproto.MIMEHeader) error {
		if code == http.StatusEarlyHints {
			hint = h.Get("Link")
		}
		return nil
	}}
	req, _ := http.NewRequest("GET", url, nil)
	resp, err := http.DefaultClient.Do(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return hint, resp, string(body)
}

func TestEarlyHints(t *testing.T) {
	base := startStreaming(t, "server.php")
	hint, resp, body := hinted(t, base+"/hints")
	if hint != "</style.css>; rel=preload; as=style" {
		t.Errorf("103 Link = %q", hint)
	}
	if resp.StatusCode != 200 || !strings.Contains(body, "<p>Hello</p>") || resp.Header.Get("Link") != "" {
		t.Errorf("answer: %d %q, Link %q", resp.StatusCode, body, resp.Header.Get("Link"))
	}
}

// What runs after the answer waits as a handler does.
func TestGoOperationAfterTheAnswer(t *testing.T) {
	base := startStreaming(t, "server.php")
	if body := fetchBody(t, base+"/after"); body != "answered" {
		t.Fatalf("body = %q", body)
	}
	if ip := seen(t, base, "after"); net.ParseIP(ip) == nil {
		t.Errorf("the lookup after the answer gave %q", ip)
	}
}

// Go sends a file that an answer names, if it is in a declared directory.
func TestFileNamedByAnAnswer(t *testing.T) {
	base := startStreaming(t, "server.php")
	const content = "A file that Go sends: PHP only named it.\n"

	resp, err := http.Get(base + "/file")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != content || resp.Header.Get("Accept-Ranges") != "bytes" || resp.Header.Get("X-Sendfile") != "" ||
		!strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		t.Errorf("file: %q, headers %v", body, resp.Header)
	}

	for _, tc := range []struct {
		name, path, header, value string
		status                    int
		body                      string
	}{
		{"a range of a file named as nginx has it", "/file/accel", "Range", "bytes=2-5", 206, "file"},
		{"a file the client has", "/file", "If-Modified-Since", resp.Header.Get("Last-Modified"), 304, ""},
		{"a file outside the directory", "/file/outside", "", "", 404, "not found\n"},
	} {
		req, _ := http.NewRequest("GET", base+tc.path, nil)
		if tc.header != "" {
			req.Header.Set(tc.header, tc.value)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.status || string(body) != tc.body || resp.Header.Get("X-Accel-Redirect") != "" {
			t.Errorf("%s: %d %q, want %d %q", tc.name, resp.StatusCode, body, tc.status, tc.body)
		}
		if tc.status == 206 && resp.Header.Get("Content-Type") != "text/x-report" {
			t.Errorf("%s: the answer's content type became %q", tc.name, resp.Header.Get("Content-Type"))
		}
	}
}

// A body is read while it is sent: one larger than a whole body may be,
// and one whose client waits for each answer before it goes on.
func TestStreamedRequestBody(t *testing.T) {
	base := startStreaming(t, "server.php")

	big := bytes.Repeat([]byte("0123456789abcdef"), 40<<16) // 40 MiB
	sum := sha256.Sum256(big)
	for _, chunked := range []bool{false, true} {
		var body io.Reader = bytes.NewReader(big)
		if chunked {
			body = io.MultiReader(body) // hides the length
		}
		resp, err := http.Post(base+"/upload", "application/octet-stream", body)
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			Bytes  int
			SHA256 string
			Whole  string
		}
		err = json.NewDecoder(resp.Body).Decode(&got)
		resp.Body.Close()
		if err != nil || got.Bytes != len(big) || got.SHA256 != hex.EncodeToString(sum[:]) || got.Whole != "" {
			t.Errorf("chunked %v: %+v (%v)", chunked, got, err)
		}
	}

	in, out := io.Pipe()
	req, _ := http.NewRequest("POST", base+"/echo", in)
	done := make(chan *http.Response, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
		}
		done <- resp
	}()
	fmt.Fprint(out, "one\n")
	resp := <-done
	if resp == nil {
		t.FailNow()
	}
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)
	if line, _ := r.ReadString('\n'); line != "ONE\n" {
		t.Fatalf("the first answer = %q", line)
	}
	fmt.Fprint(out, "two\n")
	if line, _ := r.ReadString('\n'); line != "TWO\n" {
		t.Fatalf("the second answer = %q", line)
	}
	out.Close()
	if rest, err := io.ReadAll(r); len(rest) != 0 || err != nil {
		t.Errorf("after the client ended: %q (%v)", rest, err)
	}
}

// Classic PHP in isolated mode: echo, header() and flush() are the answer.
func TestClassicOutputIsTheAnswer(t *testing.T) {
	finished := filepath.Join(t.TempDir(), "finished")
	base := startStreaming(t, "classic.php", "CAIRO_FINISHED="+finished)

	start := time.Now()
	resp, err := http.Get(base + "/flush?wait=0.5")
	if err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(resp.Body)
	first, _ := r.ReadString('\n')
	early := time.Since(start)
	rest, _ := io.ReadAll(r)
	resp.Body.Close()
	if first != "first\n" || string(rest) != "second\n" || early > 400*time.Millisecond {
		t.Errorf("flush(): %q after %v, then %q", first, early, rest)
	}
	if resp.Header.Get("X-Classic") != "yes" || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		t.Errorf("headers = %v", resp.Header)
	}

	resp, err = http.Get(base + "/unbuffered")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "one\ntwo\nheaders were sent\n" || resp.Header.Get("X-Late") != "" {
		t.Errorf("without buffers: %q, X-Late %q", body, resp.Header.Get("X-Late"))
	}

	// Without flush() the answer is whole and has its length.
	resp, err = http.Get(base + "/whole")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "in a buffer of its own, left open" || resp.ContentLength != int64(len(body)) {
		t.Errorf("whole: %q, length %d", body, resp.ContentLength)
	}

	hint, resp, page := hinted(t, base+"/hints")
	if hint != "</style.css>; rel=preload; as=style" || resp.StatusCode != 200 || !strings.Contains(page, "stylesheet") {
		t.Errorf("headers_send(103): hint %q, then %d %q", hint, resp.StatusCode, page)
	}

	start = time.Now()
	resp, err = http.Get(base + "/finish")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "accepted\n" || resp.ContentLength != 9 || time.Since(start) > 250*time.Millisecond {
		t.Errorf("fastcgi_finish_request(): %q, length %d, after %v", body, resp.ContentLength, time.Since(start))
	}
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if note, _ := os.ReadFile(finished); string(note) == "finished, then worked\n" {
			break
		} else if time.Now().After(end) {
			t.Fatalf("the handler did not go on after the answer: %q", note)
		}
	}
}

// A framework's streamed response is sent while its callback writes it, a
// file response is sent in pieces with the ranges Symfony makes of it, and
// a large PSR-7 body is too.
func TestFrameworkStreamedResponses(t *testing.T) {
	base := startFrameworks(t)

	start := time.Now()
	resp, err := http.Get(base + "/sf/stream")
	if err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(resp.Body)
	first, _ := r.ReadString('\n')
	early := time.Since(start)
	rest, err := io.ReadAll(r)
	resp.Body.Close()
	if first != "part 1\n" || string(rest) != "part 2\n" || err != nil || early > 250*time.Millisecond {
		t.Errorf("streamed response: %q after %v, then %q (%v)", first, early, rest, err)
	}

	file, err := os.ReadFile("../examples/frameworks/composer.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		rng    string
		status int
		want   []byte
	}{{"", 200, file}, {"bytes=2-9", 206, file[2:10]}} {
		req, _ := http.NewRequest("GET", base+"/sf/file", nil)
		if tc.rng != "" {
			req.Header.Set("Range", tc.rng)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.status || !bytes.Equal(body, tc.want) || err != nil || resp.ContentLength != int64(len(tc.want)) {
			t.Errorf("file response, range %q: %d, %d bytes of %d (%v)", tc.rng, resp.StatusCode, len(body), len(tc.want), err)
		}
	}

	resp, err = http.Get(base + "/psr/large")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if want := strings.Repeat("0123456789abcde\n", 3<<16); string(body) != want || err != nil {
		t.Errorf("a large PSR-7 body: %d bytes of %d (%v)", len(body), len(want), err)
	}
}
