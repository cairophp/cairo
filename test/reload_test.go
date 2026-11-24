package test

import (
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// reloadApp is examples/reload, served from a copy that a test can change.
type reloadApp struct {
	t      *testing.T
	dir    string // where the copy is
	addr   string
	cmd    *exec.Cmd
	logs   syncBuffer
	client *http.Client
}

// copyReload copies examples/reload to dir.
func copyReload(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"server.php", "greeting.php"} {
		code, err := os.ReadFile(filepath.Join("../examples/reload", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), code, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// startReload serves a copy of examples/reload.
func startReload(t *testing.T, args ...string) *reloadApp {
	t.Helper()
	dir := t.TempDir()
	copyReload(t, dir)
	return serveReload(t, dir, nil, args...)
}

// serveReload serves the copy of examples/reload in dir, which may be a
// link to it.
func serveReload(t *testing.T, dir string, env []string, args ...string) *reloadApp {
	t.Helper()
	app := &reloadApp{t: t, dir: dir, addr: freeAddr(), client: &http.Client{Timeout: 10 * time.Second}}
	app.cmd = exec.Command(cairoBin, append([]string{"serve", filepath.Join(dir, "server.php"), "--offload-threads", "1"}, args...)...)
	app.cmd.Env = append(append(os.Environ(), "CAIRO_HTTP="+app.addr), env...)
	app.cmd.Stdout, app.cmd.Stderr = os.Stderr, io.MultiWriter(os.Stderr, &app.logs)
	if err := app.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		app.client.CloseIdleConnections()
		_ = app.cmd.Process.Signal(os.Interrupt)
		_ = app.cmd.Wait()
	})
	if err := waitFor(app.addr, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	return app
}

// get is the status and the body of a GET, or 0 when the server did not
// answer.
func (a *reloadApp) get(path string) (int, string) {
	resp, err := a.client.Get("http://" + a.addr + path)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// slow begins a request that waits in PHP, and returns once PHP has begun
// to answer it. The function it returns waits for the rest of the answer.
func (a *reloadApp) slow(seconds string) (rest func() string) {
	a.t.Helper()
	resp, err := a.client.Get("http://" + a.addr + "/slow?seconds=" + seconds)
	if err != nil {
		a.t.Fatal(err)
	}
	return func() string {
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return err.Error()
		}
		return string(b)
	}
}

// answers waits until path answers with body.
func (a *reloadApp) answers(path, body string) {
	a.t.Helper()
	var code int
	var got string
	waitUntil(a.t, path+" answers "+body, func() bool {
		code, got = a.get(path)
		return code == 200 && got == body
	})
}

// logged waits until the server's log has text, n times.
func (a *reloadApp) logged(text string, n int) {
	a.t.Helper()
	waitUntil(a.t, "the log says "+text, func() bool { return strings.Count(a.logs.String(), text) >= n })
}

// greet changes the text of greeting() on disk.
func (a *reloadApp) greet(text string) {
	a.t.Helper()
	a.write("greeting.php", "<?php\n\nfunction greeting(): string\n{\n    return '"+text+"';\n}\n")
}

// write replaces a file of the app the way an editor or a deploy does: the
// whole file at once.
func (a *reloadApp) write(name, code string) {
	a.t.Helper()
	next := filepath.Join(a.dir, name+".next")
	if err := os.WriteFile(next, []byte(code), 0o644); err != nil {
		a.t.Fatal(err)
	}
	if err := os.Rename(next, filepath.Join(a.dir, name)); err != nil {
		a.t.Fatal(err)
	}
}

// declare adds a route to server.php: another declaration than the servers
// were built from.
func (a *reloadApp) declare(path, body string) {
	a.t.Helper()
	code, err := os.ReadFile(filepath.Join(a.dir, "server.php"))
	if err != nil {
		a.t.Fatal(err)
	}
	route := "$http->get('" + path + "', fn (): Response => Response::text('" + body + "'));\n\nreturn $app;"
	a.write("server.php", strings.Replace(string(code), "return $app;", route, 1))
}

// hammer sends requests to path, one after the other, until the function it
// returns is called, which reports the answers that were not 200 with one of
// the bodies.
func (a *reloadApp) hammer(path string, bodies ...string) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for sent := 0; ; sent++ {
			select {
			case <-done:
				if sent < 10 {
					a.t.Errorf("%s: only %d requests were sent", path, sent)
				}
				return
			default:
			}
			code, body := a.get(path)
			ok := code == 200
			for _, want := range bodies {
				ok = ok && body != want
			}
			if code != 200 || ok {
				a.t.Errorf("%s during the reload: %d %q, want 200 and one of %q", path, code, body, bodies)
				return
			}
		}
	}()
	return func() { close(done); wg.Wait() }
}

// With --watch, a changed file is the code of the next request, no request
// is refused or fails while the threads are replaced, the offload threads
// get the code too, and a request that waits in PHP ends with the code it
// began with.
func TestWatchReloadsTheCode(t *testing.T) {
	for _, mode := range []string{"fiber", "isolated"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			copyReload(t, dir)
			app := serveReload(t, dir, []string{"CAIRO_CONCURRENCY=" + mode}, "--threads", "3", "--watch")
			app.answers("/", "hello")
			app.answers("/offload", "hello")

			// Isolated mode has one request on a thread: one may wait there.
			var held []func() string
			for n := map[string]int{"fiber": 6, "isolated": 1}[mode]; n > 0; n-- {
				held = append(held, app.slow("1.5"))
			}
			stopRoot, stopOffload := app.hammer("/", "hello", "changed"), app.hammer("/offload", "hello", "changed")
			app.greet("changed")
			app.logged("php code reloaded", 1)
			app.answers("/", "changed")
			app.answers("/offload", "changed")
			stopRoot()
			stopOffload()
			for _, rest := range held {
				if body := rest(); body != "began hello" {
					t.Errorf("a request that began before the reload answered %q, want the code it began with", body)
				}
			}

			// A second change, right after the first.
			app.greet("again")
			app.logged("php code reloaded", 2)
			app.answers("/", "again")
			app.answers("/offload", "again")
			if log := app.logs.String(); strings.Contains(log, "level=ERROR") || strings.Contains(log, "level=WARN") {
				t.Errorf("the reloads logged a warning or an error:\n%s", log)
			}
		})
	}
}

// Code that does not start replaces nothing: the server runs on with the
// code it has, says why, and takes the next change.
func TestReloadKeepsTheCodeThatRuns(t *testing.T) {
	app := startReload(t, "--threads", "2", "--watch")
	app.answers("/", "hello")

	app.write("greeting.php", "<?php\n\nfunction greeting(): string\n{\n    return 'broken'\n}\n")
	app.logged("php code not reloaded", 1)
	if log := app.logs.String(); !strings.Contains(log, "Parse error") {
		t.Errorf("the log does not say why the code did not start:\n%s", log)
	}
	for i := 0; i < 20; i++ {
		if code, body := app.get("/"); code != 200 || body != "hello" {
			t.Fatalf("after the refused reload: %d %q, want the old code", code, body)
		}
	}
	if code, body := app.get("/offload"); code != 200 || body != "hello" {
		t.Errorf("/offload after the refused reload: %d %q, want the old code", code, body)
	}

	app.greet("fixed")
	app.answers("/", "fixed")
}

// A changed declaration (here, a new route) needs a new process: --watch
// stops the server as a signal would and starts it anew in the same process.
func TestWatchRestartsForANewDeclaration(t *testing.T) {
	app := startReload(t, "--threads", "1", "--watch")
	app.answers("/", "hello")
	app.declare("/added", "added")
	app.answers("/added", "added")
	app.logged("restarting", 1)
	app.logged("watching for changes", 2) // the new process watches too

	app.greet("changed")
	app.answers("/", "changed")
	if log := app.logs.String(); strings.Contains(log, "level=ERROR") {
		t.Errorf("the restart logged an error:\n%s", log)
	}
}

// SIGUSR1 reloads once, for a deploy. It never restarts: a changed
// declaration is refused, and the server runs on.
func TestReloadSignal(t *testing.T) {
	app := startReload(t, "--threads", "2")
	app.answers("/", "hello")

	app.greet("changed")
	if code, body := app.get("/"); code != 200 || body != "hello" {
		t.Fatalf("before the signal: %d %q, want the code the server started with", code, body)
	}
	stop := app.hammer("/", "hello", "changed")
	if err := app.cmd.Process.Signal(syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	app.logged("php code reloaded", 1)
	stop()
	// Every thread has the code once the reload is done.
	for i := 0; i < 20; i++ {
		if code, body := app.get("/"); code != 200 || body != "changed" {
			t.Fatalf("after the reload: %d %q", code, body)
		}
	}
	if code, body := app.get("/offload"); code != 200 || body != "changed" {
		t.Errorf("/offload after the reload: %d %q", code, body)
	}

	app.declare("/added", "added")
	if err := app.cmd.Process.Signal(syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	app.logged("php code not reloaded", 1)
	app.logged("restart it", 1)
	if code, body := app.get("/"); code != 200 || body != "changed" {
		t.Errorf("after the refused reload: %d %q", code, body)
	}
	if code, _ := app.get("/added"); code != 404 {
		t.Errorf("/added after the refused reload: status %d, want 404", code)
	}
}

// A deploy that points a link at a new release and sends the signal is
// served from the new release.
func TestReloadFollowsALink(t *testing.T) {
	root := t.TempDir()
	for _, release := range []string{"one", "two"} {
		copyReload(t, filepath.Join(root, release))
	}
	current := filepath.Join(root, "current")
	if err := os.Symlink("one", current); err != nil {
		t.Fatal(err)
	}
	app := serveReload(t, current, nil, "--threads", "2")
	app.answers("/where", "one")

	next := filepath.Join(root, "next")
	if err := os.Symlink("two", next); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, current); err != nil {
		t.Fatal(err)
	}
	if err := app.cmd.Process.Signal(syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	app.logged("php code reloaded", 1)
	for i := 0; i < 20; i++ {
		if code, body := app.get("/where"); code != 200 || body != "two" {
			t.Fatalf("after the reload: %d %q, want the new release", code, body)
		}
	}
}

// --watch-path names what is watched instead of the server file's
// directory; the server file itself always is.
func TestWatchPath(t *testing.T) {
	dir := t.TempDir()
	copyReload(t, dir)
	app := serveReload(t, dir, nil, "--threads", "1", "--watch-path", filepath.Join(dir, "greeting.php"))
	app.answers("/", "hello")
	app.greet("changed")
	app.answers("/", "changed")
	app.declare("/added", "added")
	app.answers("/added", "added")
}
