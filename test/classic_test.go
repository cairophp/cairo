package test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// startClassic serves examples/classic (classic mode, scripts()) on the
// given number of PHP threads, with env added, and returns its base URL.
func startClassic(t *testing.T, threads int, env ...string) string {
	t.Helper()
	addr := freeAddr()
	cmd := exec.Command(cairoBin, "serve", "../examples/classic/server.php", "--offload-threads", "1")
	cmd.Env = append(append(os.Environ(), "CAIRO_HTTP="+addr, fmt.Sprintf("CAIRO_THREADS=%d", threads)), env...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		_ = cmd.Wait()
	})
	if err := waitFor(addr, 20*time.Second); err != nil {
		t.Fatal(err)
	}
	return "http://" + addr
}

func classicGet(t *testing.T, client *http.Client, url string) (*http.Response, string) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

// Each request is a PHP request of its own: what a script declared and set
// is gone for the next request, on the same thread.
func TestClassicRequestsStartFresh(t *testing.T) {
	base := startClassic(t, 1)
	for i := range 3 {
		_, body := classicGet(t, http.DefaultClient, base+"/state.php")
		if body != `{"hits":1,"defined":false,"statics":0}`+"\n" {
			t.Fatalf("request %d: %q, want a fresh PHP request", i, body)
		}
	}
	// A fatal error ends its PHP request only.
	if resp, body := classicGet(t, http.DefaultClient, base+"/fatal.php"); resp.StatusCode != 500 || !strings.HasPrefix(body, "partial") {
		t.Fatalf("fatal.php: %d %q", resp.StatusCode, body)
	}
	if _, body := classicGet(t, http.DefaultClient, base+"/state.php"); !strings.Contains(body, `"hits":1`) {
		t.Fatalf("after a fatal error: %q", body)
	}
}

func TestClassicScriptsAreRouted(t *testing.T) {
	base := startClassic(t, 1)
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, c := range []struct{ path, want string }{
		{"/", "front / script=/index.php\n"},
		{"/hello-world/", "front /hello-world/ script=/index.php\n"}, // a pretty link goes to the front controller
		{"/.env", "front /.env script=/index.php\n"},                 // hidden files are never served
		{"/blog/", "blog index\n"},
		{"/robots.txt", "User-agent: *\n"}, // a file, served by Go
		{"/end.php", "before\nshutdown\n"}, // exit() ends the script, then its shutdown functions run
	} {
		if resp, body := classicGet(t, noRedirect, base+c.path); resp.StatusCode != 200 || body != c.want {
			t.Errorf("%s: %d %q, want %q", c.path, resp.StatusCode, body, c.want)
		}
	}
	if resp, _ := classicGet(t, noRedirect, base+"/blog"); resp.StatusCode != 301 || resp.Header.Get("Location") != "/blog/" {
		t.Errorf("/blog: %d %q, want a redirect to /blog/", resp.StatusCode, resp.Header.Get("Location"))
	}
	if _, body := classicGet(t, noRedirect, base+"/state.php"); !strings.Contains(body, `"hits"`) || strings.Contains(body, "<?php") {
		t.Errorf("a script's source was sent: %q", body)
	}
}

func TestClassicRequestIsPHPs(t *testing.T) {
	base := startClassic(t, 1)
	resp, err := http.PostForm(base+"/form.php/a/b?q=1", url.Values{"name": {"Ada"}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got struct {
		Method   string
		Get      map[string]string
		Post     map[string]string
		Input    string
		PathInfo string `json:"path_info"`
		Self     string
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Method != "POST" || got.Get["q"] != "1" || got.Post["name"] != "Ada" || got.Input != "name=Ada" || got.PathInfo != "/a/b" || got.Self != "/form.php/a/b" {
		t.Errorf("form.php saw %+v", got)
	}
	if resp.Header.Get("X-Classic") != "yes" || !strings.Contains(resp.Header.Get("Set-Cookie"), "seen=1") {
		t.Errorf("header() and setcookie(): %v", resp.Header)
	}

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	for i := 1; i <= 3; i++ {
		if _, body := classicGet(t, client, base+"/session.php"); body != fmt.Sprintf("visits=%d\n", i) {
			t.Fatalf("session visit %d: %q", i, body)
		}
	}
}

// A classic thread runs one request at a time, but a request's own fibers
// wait together: on 2 threads, 4 requests that each wait on two fibers of
// 0.5 s take about 1 s, not 2 or 4.
func TestClassicWaitsSuspendOnlyTheFiber(t *testing.T) {
	base := startClassic(t, 2)
	classicGet(t, http.DefaultClient, base+"/") // the threads are up
	start := time.Now()
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if _, body := classicGet(t, http.DefaultClient, base+"/wait.php"); !strings.HasPrefix(body, "ab ") {
				t.Errorf("wait.php: %q", body)
			}
		})
	}
	wg.Wait()
	if elapsed := time.Since(start); elapsed > 1600*time.Millisecond {
		t.Errorf("4 requests of two 0.5 s waits on 2 threads took %v", elapsed)
	}
}

// A classic request runs on its thread's own stack, and Cairo's waits work
// there as they do in a fiber: the fibers the script started run while it
// waits, and flush() sends its answer on through Go.
func TestClassicWaitsOnItsOwnStack(t *testing.T) {
	base := startClassic(t, 1)
	_, body := classicGet(t, http.DefaultClient, base+"/together.php")
	var got struct {
		SentAfter int `json:"sent_after_ms"`
		Timeout   any
		Derived   string
		Offload   string
		Ms        int
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("together.php: %q: %v", body, err)
	}
	// The task sent after 0.1 s, while the script slept for 0.2 s.
	if got.SentAfter > 180 || got.Timeout != nil || got.Derived != "deadline" || got.Offload != "ABC" || got.Ms > 450 {
		t.Errorf("together.php: %+v", got)
	}

	start := time.Now()
	resp, err := http.Get(base + "/stream.php")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)
	first, _ := r.ReadString('\n')
	early := time.Since(start)
	rest, _ := io.ReadAll(r)
	if first != "first\n" || string(rest) != "second\n" || early > 400*time.Millisecond {
		t.Errorf("flush(): %q after %v, then %q", first, early, rest)
	}
}

// The deadline stops a classic script where it runs, in a loop that never
// waits and in PHP's own waits, which hold the thread: the thread serves the
// next request right away.
func TestClassicDeadlineStopsTheScript(t *testing.T) {
	peer, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { peer.Close() })
	go func() {
		for {
			conn, err := peer.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { conn.Close() })
		}
	}()
	base := startClassic(t, 1, "CAIRO_TIMEOUT=0.5", "CAIRO_SILENT_PEER="+peer.Addr().String())
	for _, path := range []string{"/spin.php", "/held.php", "/held.php?read=1"} {
		start := time.Now()
		if resp, _ := classicGet(t, http.DefaultClient, base+path); resp.StatusCode != 504 || time.Since(start) > 2*time.Second {
			t.Errorf("%s: %d after %v, want 504 at the deadline", path, resp.StatusCode, time.Since(start))
		}
		start = time.Now()
		if _, body := classicGet(t, http.DefaultClient, base+"/state.php"); !strings.Contains(body, `"hits":1`) || time.Since(start) > 300*time.Millisecond {
			t.Errorf("after %s: %q after %v", path, body, time.Since(start))
		}
	}
}

// putenv() is the request's own: getenv() sees it in that request only, and
// many requests that call it at once on many threads (Laravel's .env loader
// does, 47 times a request) do not crash the process, as PHP's putenv() of
// the process's environment did.
func TestClassicPutenvIsTheRequests(t *testing.T) {
	base := startClassic(t, 8)
	want := `{"before":false,"after":"7","home":false,"listed":"7"}` + "\n"
	for range 3 {
		if _, body := classicGet(t, http.DefaultClient, base+"/env.php?n=7"); body != want {
			t.Fatalf("env.php: %q, want %q", body, want)
		}
	}
	var wg sync.WaitGroup
	for w := range 32 {
		wg.Go(func() {
			for i := range 50 {
				n := fmt.Sprint(w*100 + i)
				resp, body := classicGet(t, http.DefaultClient, base+"/env.php?n="+n)
				if resp.StatusCode != 200 || !strings.Contains(body, `"before":false,"after":"`+n+`"`) {
					t.Errorf("request %s: %d %q", n, resp.StatusCode, body)
					return
				}
			}
		})
	}
	wg.Wait()
}
