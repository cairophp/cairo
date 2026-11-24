package test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// startSymfony serves examples/symfony through its own front controller,
// with no server.php, and the runtime options in options (JSON, without the
// address). It returns the base URL and the server's log.
func startSymfony(t *testing.T, options string) (string, *syncBuffer) {
	t.Helper()
	dir := "../examples/symfony"
	composerInstall(t, dir)
	if _, err := os.Stat(dir + "/vendor/autoload_runtime.php"); err != nil {
		t.Skip("composer did not run Symfony's runtime plugin, which writes vendor/autoload_runtime.php")
	}
	addr := freeAddr()
	if options != "" {
		options = "," + options
	}
	logs := &syncBuffer{}
	cmd := exec.Command(cairoBin, "serve", dir+"/public/index.php", "--offload-threads", "1")
	cmd.Env = append(os.Environ(), fmt.Sprintf(`APP_RUNTIME_OPTIONS={"listen":%q%s}`, addr, options))
	cmd.Stdout, cmd.Stderr = os.Stderr, io.MultiWriter(os.Stderr, logs)
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
	return "http://" + addr, logs
}

func getBody(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestSymfonyFrontControllerIsServed(t *testing.T) {
	base, _ := startSymfony(t, "")

	if code, body := getBody(t, base+"/hello/cairo"); code != 200 || body != "Hello cairo from Symfony\n" {
		t.Fatalf("/hello/cairo: %d %q", code, body)
	}
	// public/ is the document root, served by Go.
	if code, body := getBody(t, base+"/robots.txt"); code != 200 || !strings.HasPrefix(body, "User-agent") {
		t.Fatalf("/robots.txt: %d %q", code, body)
	}
	if code, _ := getBody(t, base+"/no-such-route"); code != 404 {
		t.Fatalf("an unknown route: %d, want Symfony's 404", code)
	}
	resp, err := http.PostForm(base+"/form", url.Values{"name": {"Ada"}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var form struct{ Name string }
	if err := json.NewDecoder(resp.Body).Decode(&form); err != nil || form.Name != "Ada" {
		t.Fatalf("POST /form: %v %+v", err, form)
	}
	var visit struct{ Runtime string }
	_, body := getBody(t, base+"/visit/x")
	if err := json.Unmarshal([]byte(body), &visit); err != nil || visit.Runtime != `Cairo\Symfony\Runtime` {
		t.Fatalf("/visit: %v %s", err, body)
	}
}

// In fiber mode each request in flight on a thread has a kernel of its own:
// the request state in a kernel's services is not another request's.
func TestSymfonyFibersGiveEachRequestItsKernel(t *testing.T) {
	base, _ := startSymfony(t, `"concurrency":"fibers","threads":1`)
	getBody(t, base+"/hello/warm") // the first kernel has booted

	const n = 20
	type visit struct {
		Visitor string
		Kernel  int
	}
	visits := make([]visit, n)
	var wg sync.WaitGroup
	start := time.Now()
	for i := range n {
		wg.Go(func() {
			code, body := getBody(t, fmt.Sprintf("%s/visit/v%d?wait=500000", base, i))
			if code != 200 || json.Unmarshal([]byte(body), &visits[i]) != nil {
				t.Errorf("visit %d: %d %s", i, code, body)
			}
		})
	}
	wg.Wait()
	elapsed := time.Since(start)
	kernels := map[int]bool{}
	for i, v := range visits {
		if v.Visitor != fmt.Sprintf("v%d", i) {
			t.Errorf("request %d saw visitor %q: another request's kernel", i, v.Visitor)
		}
		kernels[v.Kernel] = true
	}
	if len(kernels) != n {
		t.Errorf("%d requests in flight together had %d kernels", n, len(kernels))
	}
	// Each waits 0.5 s, on one PHP thread: together, not one after another.
	if elapsed > 2*time.Second {
		t.Errorf("%d requests of 0.5 s on one thread took %v", n, elapsed)
	}
}

// Symfony's front controller ends the thread's script with exit() once the
// runner returns: a recycled thread is still a clean end.
func TestSymfonyThreadsAreRecycled(t *testing.T) {
	base, logs := startSymfony(t, `"threads":1,"max_requests":3`)
	for i := range 12 {
		want := fmt.Sprintf("Hello r%d from Symfony\n", i)
		if code, body := getBody(t, fmt.Sprintf("%s/hello/r%d", base, i)); code != 200 || body != want {
			t.Fatalf("request %d: %d %q", i, code, body)
		}
	}
	waitUntil(t, "a thread is recycled", func() bool { return strings.Contains(logs.String(), "php thread recycled") })
	if strings.Contains(logs.String(), "stopped unexpectedly") {
		t.Fatalf("a recycled thread was reported as a crash:\n%s", logs.String())
	}
}
