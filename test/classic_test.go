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
	"path/filepath"
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
	return startScripts(t, addr, []string{"../examples/classic/server.php", "--offload-threads", "1"},
		append([]string{"CAIRO_HTTP=" + addr, fmt.Sprintf("CAIRO_THREADS=%d", threads)}, env...))
}

// startPlainClassic serves examples/classic's site/ with no declaration, as
// PHP-FPM serves a folder: what server.php declares, from the command line.
func startPlainClassic(t *testing.T, threads int, env ...string) string {
	t.Helper()
	addr := freeAddr()
	return startScripts(t, addr, []string{"../examples/classic/site", "--listen", addr, "--threads", fmt.Sprint(threads), "--offload-threads", "1"}, env)
}

// classicWays are the two ways to serve examples/classic: the same scripts
// must behave the same under both.
var classicWays = []struct {
	name  string
	start func(t *testing.T, threads int, env ...string) string
}{{"declared", startClassic}, {"plain", startPlainClassic}}

// eachClassic runs test on examples/classic served each way, on the given
// number of PHP threads.
func eachClassic(t *testing.T, threads int, test func(t *testing.T, base string)) {
	for _, way := range classicWays {
		t.Run(way.name, func(t *testing.T) { test(t, way.start(t, threads)) })
	}
}

// startScripts runs cairo serve with args and env added, and returns the
// base URL once addr answers.
func startScripts(t *testing.T, addr string, args, env []string) string {
	t.Helper()
	cmd := exec.Command(cairoBin, append([]string{"serve"}, args...)...)
	cmd.Env = append(os.Environ(), env...)
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
	eachClassic(t, 1, func(t *testing.T, base string) {
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
	})
}

func TestClassicScriptsAreRouted(t *testing.T) {
	eachClassic(t, 1, func(t *testing.T, base string) {
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
	})
}

func TestClassicRequestIsPHPs(t *testing.T) {
	eachClassic(t, 1, func(t *testing.T, base string) {
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
	})
}

// A classic thread runs one request at a time, but a request's own fibers
// wait together: on 2 threads, 4 requests that each wait on two fibers of
// 0.5 s take about 1 s, not 2 or 4.
func TestClassicWaitsSuspendOnlyTheFiber(t *testing.T) {
	eachClassic(t, 2, func(t *testing.T, base string) {
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
	})
}

// A classic request runs on its thread's own stack, and Cairo's waits work
// there as they do in a fiber: the fibers the script started run while it
// waits, and flush() sends its answer on through Go.
func TestClassicWaitsOnItsOwnStack(t *testing.T) {
	eachClassic(t, 1, func(t *testing.T, base string) {
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
	})
}

// The deadline (Middleware::timeout(), or with no declaration --timeout or
// php.ini's max_execution_time)
// stops a classic script where it runs, in a loop that never waits and in
// PHP's own waits, which hold the thread: the thread serves the next request
// right away.
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
	silent := "CAIRO_SILENT_PEER=" + peer.Addr().String()
	starts := map[string]func() string{
		"declared": func() string { return startClassic(t, 1, "CAIRO_TIMEOUT=0.5", silent) },
		"plain": func() string {
			addr := freeAddr()
			return startScripts(t, addr, []string{"../examples/classic/site", "--listen", addr, "--threads", "1", "--offload-threads", "1", "--timeout", "500ms"}, []string{silent})
		},
		// With no --timeout, php.ini's max_execution_time is the deadline.
		"php.ini": func() string {
			ini := filepath.Join(t.TempDir(), "php.ini")
			if err := os.WriteFile(ini, []byte("max_execution_time = 1\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			addr := freeAddr()
			return startScripts(t, addr, []string{"../examples/classic/site", "--listen", addr, "--threads", "1", "--offload-threads", "1", "--php-ini", ini}, []string{silent})
		},
	}
	for name, start := range starts {
		t.Run(name, func(t *testing.T) {
			base := start()
			for _, path := range []string{"/spin.php", "/held.php", "/held.php?read=1"} {
				begin := time.Now()
				if resp, _ := classicGet(t, http.DefaultClient, base+path); resp.StatusCode != 504 || time.Since(begin) > 2*time.Second {
					t.Errorf("%s: %d after %v, want 504 at the deadline", path, resp.StatusCode, time.Since(begin))
				}
				begin = time.Now()
				if _, body := classicGet(t, http.DefaultClient, base+"/state.php"); !strings.Contains(body, `"hits":1`) || time.Since(begin) > 300*time.Millisecond {
					t.Errorf("after %s: %q after %v", path, body, time.Since(begin))
				}
			}
		})
	}
}
