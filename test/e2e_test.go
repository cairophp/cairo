// End-to-end tests: build the cairo binary, run examples/greeter on ONE PHP
// thread, and check milestone 1 from the outside, over real HTTP and gRPC.
//
// Requires a ZTS PHP with the embed SAPI; see the README for CGO_CFLAGS and
// CGO_LDFLAGS. Run with: go test ./test/ -v
package test

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bufbuild/protocompile"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

var (
	httpAddr string
	grpcAddr string
	cairoBin string // the binary built for this test run
)

func TestMain(m *testing.M) {
	code, err := run(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e setup:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func run(m *testing.M) (int, error) {
	dir, err := os.MkdirTemp("", "cairo-e2e-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(dir)
	bin := filepath.Join(dir, "cairo")
	cairoBin = bin
	build := exec.Command("go", "build", "-o", bin, "../cmd/cairo")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		return 0, fmt.Errorf("build cairo: %w", err)
	}

	httpAddr, grpcAddr = freeAddr(), freeAddr()
	cmd := exec.Command(bin, "serve", "../examples/greeter/server.php", "--threads", "1", "--offload-threads", "1")
	cmd.Env = append(os.Environ(), "CAIRO_HTTP="+httpAddr, "CAIRO_GRPC="+grpcAddr)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	defer func() {
		_ = cmd.Process.Signal(os.Interrupt)
		_ = cmd.Wait()
	}()
	if err := waitFor(httpAddr, 10*time.Second); err != nil {
		return 0, err
	}
	if err := waitFor(grpcAddr, 10*time.Second); err != nil {
		return 0, err
	}
	return m.Run(), nil
}

func TestHTTPRouteAnswersFromPHP(t *testing.T) {
	body := get(t, "/hello/Mohamed")
	if body != "Hello Mohamed\n" {
		t.Fatalf("got %q", body)
	}
}

// The core promise: one PHP thread holds 1,000 requests at once. Each request
// sleeps 1s in Cairo\sleep(); its fiber suspends while a goroutine waits, so
// all of them finish in about 1s instead of 1,000s.
func TestOneThreadServesThousandConcurrentRequests(t *testing.T) {
	const n = 1000
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: n, MaxConnsPerHost: n}}
	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := client.Get("http://" + httpAddr + "/sleep?seconds=1")
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 200 || !strings.Contains(string(b), `"slept":1`) {
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
	t.Logf("%d concurrent requests, each sleeping 1s, on 1 PHP thread: %s", n, took.Round(time.Millisecond))
	if took > 3*time.Second {
		t.Fatalf("took %s; requests were not concurrent", took)
	}
}

// CPU-heavy work sent with Cairo\offload() runs on the offload pool, so the
// single request thread keeps answering other requests meanwhile.
func TestOffloadKeepsRequestThreadResponsive(t *testing.T) {
	done := make(chan string, 1)
	go func() { done <- get(t, "/cpu?seconds=1.5") }()
	time.Sleep(200 * time.Millisecond) // the CPU job is now running

	start := time.Now()
	if body := get(t, "/hello/during-cpu"); body != "Hello during-cpu\n" {
		t.Fatalf("got %q", body)
	}
	if took := time.Since(start); took > 300*time.Millisecond {
		t.Fatalf("request thread was blocked for %s while a CPU job ran", took)
	}
	if body := <-done; !strings.Contains(body, `"iterations":`) {
		t.Fatalf("offload result: %q", body)
	}
}

// Runtime hooks: handlers call PHP's own usleep(), unchanged, and 500 of them
// still overlap on one PHP thread.
func TestHookedUsleepDoesNotBlockTheThread(t *testing.T) {
	const n = 500
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: n, MaxConnsPerHost: n}}
	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := client.Get("http://" + httpAddr + "/blocking-sleep?seconds=1")
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 200 || !strings.Contains(string(b), `"slept":1`) {
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
	t.Logf("%d requests calling usleep(1s) on 1 PHP thread: %s", n, took.Round(time.Millisecond))
	if took > 3*time.Second {
		t.Fatalf("took %s; usleep() blocked the thread", took)
	}
}

// Socket hooks: 200 requests each open a plain PHP stream to a server that
// answers after 1s. Their reads suspend fibers, so they overlap.
func TestHookedSocketReadsDoNotBlockTheThread(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				var secs float64
				fmt.Fscanln(c, &secs)
				time.Sleep(time.Duration(secs * float64(time.Second)))
				fmt.Fprintln(c, "pong")
			}()
		}
	}()

	const n = 200
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: n, MaxConnsPerHost: n}}
	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := client.Get("http://" + httpAddr + "/tcp?delay=1&addr=" + ln.Addr().String())
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 200 || !strings.Contains(string(b), `"pong"`) {
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
	t.Logf("%d requests waiting 1s on a PHP stream socket, on 1 PHP thread: %s", n, took.Round(time.Millisecond))
	if took > 3*time.Second {
		t.Fatalf("took %s; socket reads blocked the thread", took)
	}
}

// Connect hooks: unix sockets work, a refused connect reports an error like
// PHP does, and DNS goes through Go's resolver.
func TestHookedConnectAndDNS(t *testing.T) {
	path := filepath.Join(t.TempDir(), "echo.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				var secs float64
				fmt.Fscanln(c, &secs)
				fmt.Fprintln(c, "unix-pong")
			}()
		}
	}()
	if body := get(t, "/tcp?delay=0&addr=unix://"+path); !strings.Contains(body, `"unix-pong"`) {
		t.Fatalf("unix: %s", body)
	}

	closed := freeAddr()
	resp, err := http.Get("http://" + httpAddr + "/tcp?addr=" + closed)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 502 || !strings.Contains(string(b), "refused") {
		t.Fatalf("refused connect: status %d body %s", resp.StatusCode, b)
	}

	var dns struct {
		One string   `json:"one"`
		All []string `json:"all"`
	}
	if err := json.Unmarshal([]byte(get(t, "/dns/localhost")), &dns); err != nil {
		t.Fatal(err)
	}
	if dns.One != "127.0.0.1" || len(dns.All) == 0 || dns.All[0] != "127.0.0.1" {
		t.Fatalf("dns: %+v", dns)
	}
}

// A listener that never accepts, with a backlog of 0: once the kernel's queue
// is full, further connects hang until they time out. 30 such connects with a
// 1s timeout overlap on one PHP thread instead of taking 30s.
func TestHookedConnectDoesNotBlockTheThread(t *testing.T) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Listen(fd, 0); err != nil {
		t.Fatal(err)
	}
	sa, _ := syscall.Getsockname(fd)
	addr := fmt.Sprintf("127.0.0.1:%d", sa.(*syscall.SockaddrInet4).Port)

	const n = 30
	start := time.Now()
	var wg sync.WaitGroup
	var mu sync.Mutex
	timedOut := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var out struct {
				Connected bool `json:"connected"`
			}
			if err := json.Unmarshal([]byte(get(t, "/connect?timeout=1&addr="+addr)), &out); err != nil {
				t.Error(err)
				return
			}
			if !out.Connected {
				mu.Lock()
				timedOut++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	took := time.Since(start)
	t.Logf("%d connects to a full backlog (%d timed out after 1s), on 1 PHP thread: %s", n, timedOut, took.Round(time.Millisecond))
	if timedOut == 0 {
		t.Fatal("no connect hung; the test listener did not fill up")
	}
	if took > 4*time.Second {
		t.Fatalf("took %s; connects blocked the thread", took)
	}
}

// Six tasks share a pool of two: two are opened, the tasks take turns (three
// rounds of 0.2s), and every connection goes back when its task ends.
func TestPoolGivesEachFiberItsOwnConnection(t *testing.T) {
	var out struct {
		Opened   int     `json:"opened"`
		Distinct int     `json:"distinct"`
		Took     float64 `json:"took"`
		Stats    struct {
			Open int `json:"open"`
			Idle int `json:"idle"`
		} `json:"stats"`
	}
	if err := json.Unmarshal([]byte(get(t, "/pool")), &out); err != nil {
		t.Fatal(err)
	}
	if out.Opened != 2 || out.Distinct != 2 || out.Took < 0.55 || out.Took > 1.0 || out.Stats.Idle != 2 {
		t.Fatalf("got %+v", out)
	}
}

// curl hook: 100 requests each curl a server that answers after 1s. curl's
// socket waits suspend fibers, so they overlap; curl_getinfo() still works,
// and a curl timeout still reports curl's own error.
func TestHookedCurlDoesNotBlockTheThread(t *testing.T) {
	slow := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d, _ := time.ParseDuration(r.URL.Query().Get("d"))
		time.Sleep(d)
		w.WriteHeader(201)
		fmt.Fprint(w, "slow-ok")
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go slow.Serve(ln)
	defer slow.Close()
	target := "http://" + ln.Addr().String() + "/?d="

	const n = 100
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: n, MaxConnsPerHost: n}}
	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := client.Get("http://" + httpAddr + "/curl?url=" + target + "1s")
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(b), `"body":"slow-ok"`) || !strings.Contains(string(b), `"status":201`) {
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
	t.Logf("%d curl_exec() calls to a 1s endpoint, on 1 PHP thread: %s", n, took.Round(time.Millisecond))
	if took > 3*time.Second {
		t.Fatalf("took %s; curl blocked the thread", took)
	}

	var out struct {
		Errno int `json:"errno"`
	}
	if err := json.Unmarshal([]byte(get(t, "/curl?timeout=1&url="+target+"3s")), &out); err != nil {
		t.Fatal(err)
	}
	if out.Errno != 28 { // CURLE_OPERATION_TIMEDOUT
		t.Fatalf("want curl errno 28 (timeout), got %d", out.Errno)
	}
}

// PDO MySQL through the socket hooks: 50 queries that each run SELECT SLEEP(1)
// overlap on one PHP thread. Needs CAIRO_TEST_MYSQL (a PDO DSN) and a PHP
// build with pdo_mysql; CI runs it against a MySQL service.
func TestHookedPDOMySQL(t *testing.T) {
	if os.Getenv("CAIRO_TEST_MYSQL") == "" {
		t.Skip("CAIRO_TEST_MYSQL is not set")
	}
	const n = 50
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: n, MaxConnsPerHost: n}}
	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := client.Get("http://" + httpAddr + "/mysql?seconds=1")
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 200 || !strings.Contains(string(b), `"answer":42`) {
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
	t.Logf("%d PDO queries running SELECT SLEEP(1), on 1 PHP thread: %s", n, took.Round(time.Millisecond))
	if took > 3*time.Second {
		t.Fatalf("took %s; PDO blocked the thread", took)
	}
}

func TestContextValuesFollowFibers(t *testing.T) {
	var out struct {
		User  string `json:"user"`
		Child struct {
			Seen string `json:"seen"`
			Own  string `json:"own"`
		} `json:"child"`
		Missing string `json:"missing"`
	}
	if err := json.Unmarshal([]byte(get(t, "/context/mohamed")), &out); err != nil {
		t.Fatal(err)
	}
	if out.User != "mohamed" || out.Child.Seen != "mohamed" || out.Child.Own != "child" || out.Missing != "default" {
		t.Fatalf("got %+v", out)
	}
	// The deferred callback ran when the request's fiber ended.
	if audit := get(t, "/audit"); !strings.Contains(audit, `"done mohamed"`) {
		t.Fatalf("audit log %s", audit)
	}
}

func TestAsyncAllInsideOneRequest(t *testing.T) {
	var out struct {
		Results []string `json:"results"`
		Took    float64  `json:"took"`
	}
	if err := json.Unmarshal([]byte(get(t, "/parallel")), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Join(out.Results, ",") != "a,b" || out.Took > 1.5 {
		t.Fatalf("two 1s sleeps should overlap: %+v", out)
	}
}

func TestGRPCUnaryFromPHP(t *testing.T) {
	g := newGreeter(t)
	reply, err := g.call(context.Background(), "SayHello", map[string]any{"name": "Mohamed"})
	if err != nil {
		t.Fatal(err)
	}
	if got := reply.Get(reply.Descriptor().Fields().ByName("message")).String(); got != "Hello Mohamed" {
		t.Fatalf("got %q", got)
	}
}

func TestGRPCExceptionBecomesStatus(t *testing.T) {
	g := newGreeter(t)
	_, err := g.call(context.Background(), "SayHello", map[string]any{"name": ""})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

func TestGRPCDeadlineReachesPHP(t *testing.T) {
	g := newGreeter(t)

	// PHP sees the remaining time.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	reply, err := g.call(ctx, "SlowHello", map[string]any{"name": "x", "seconds": 0.1})
	if err != nil {
		t.Fatal(err)
	}
	remaining := reply.Get(reply.Descriptor().Fields().ByName("remaining")).Float()
	if remaining <= 0 || remaining > 2 {
		t.Fatalf("PHP saw remaining=%v, want between 0 and 2", remaining)
	}

	// A handler sleeping past the deadline gets DEADLINE_EXCEEDED at the deadline.
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	start := time.Now()
	_, err = g.call(ctx2, "SlowHello", map[string]any{"name": "x", "seconds": 5.0})
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("deadline took %s", took)
	}
}

// Go-side middleware declared in server.php: request ids reach PHP and come
// back on the response, CORS preflights never reach PHP, big responses are
// gzipped, and gRPC calls get an x-request-id header.
func TestMiddleware(t *testing.T) {
	resp, err := http.Get("http://" + httpAddr + "/request-id")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if id := resp.Header.Get("X-Request-Id"); len(id) != 32 || string(b) != id {
		t.Fatalf("request id header %q, PHP saw %q", id, b)
	}

	req, _ := http.NewRequest("OPTIONS", "http://"+httpAddr+"/hello/x", nil)
	req.Header.Set("Origin", "https://app.example")
	req.Header.Set("Access-Control-Request-Method", "GET")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 || resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("preflight: %d %v", resp.StatusCode, resp.Header)
	}

	tr := &http.Transport{DisableCompression: true}
	req, _ = http.NewRequest("GET", "http://"+httpAddr+"/big", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err = tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.Header.Get("Content-Encoding") != "gzip" || len(raw) >= 5200 {
		t.Fatalf("big response: encoding %q, %d bytes on the wire", resp.Header.Get("Content-Encoding"), len(raw))
	}

	g := newGreeter(t)
	var header metadata.MD
	md := g.svc.Methods().ByName("SayHello")
	in := dynamicpb.NewMessage(md.Input())
	in.Set(md.Input().Fields().ByName("name"), protoreflect.ValueOfString("x"))
	out := dynamicpb.NewMessage(md.Output())
	if err := g.conn.Invoke(context.Background(), "/helloworld.Greeter/SayHello", in, out, grpc.Header(&header)); err != nil {
		t.Fatal(err)
	}
	if ids := header.Get("x-request-id"); len(ids) != 1 || len(ids[0]) != 32 {
		t.Fatalf("grpc x-request-id: %v", ids)
	}
}

func TestGRPCHealth(t *testing.T) {
	conn := dial(t)
	resp, err := healthpb.NewHealthClient(conn).Check(context.Background(), &healthpb.HealthCheckRequest{Service: "helloworld.Greeter"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("status %v", resp.Status)
	}
}

// ---- helpers ----

type greeter struct {
	conn *grpc.ClientConn
	svc  protoreflect.ServiceDescriptor
}

func newGreeter(t *testing.T) *greeter {
	t.Helper()
	c := protocompile.Compiler{Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{
		ImportPaths: []string{"../examples/greeter/proto"},
	})}
	files, err := c.Compile(context.Background(), "helloworld.proto")
	if err != nil {
		t.Fatal(err)
	}
	return &greeter{conn: dial(t), svc: files[0].Services().ByName("Greeter")}
}

func (g *greeter) call(ctx context.Context, method string, fields map[string]any) (*dynamicpb.Message, error) {
	md := g.svc.Methods().ByName(protoreflect.Name(method))
	in := dynamicpb.NewMessage(md.Input())
	for k, v := range fields {
		in.Set(md.Input().Fields().ByName(protoreflect.Name(k)), protoreflect.ValueOf(v))
	}
	out := dynamicpb.NewMessage(md.Output())
	err := g.conn.Invoke(ctx, "/helloworld.Greeter/"+method, in, out)
	return out, err
}

func dial(t *testing.T) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func get(t *testing.T, path string) string {
	t.Helper()
	resp, err := http.Get("http://" + httpAddr + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: status %d: %s", path, resp.StatusCode, b)
	}
	return string(b)
}

func freeAddr() string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func waitFor(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("cairo did not start listening on %s", addr)
}

// TLS through Go: 50 requests each fetch a 1s https:// endpoint with
// file_get_contents(); the PHP build has no OpenSSL. They overlap on one PHP
// thread. Certificates are verified against the context's cafile.
func TestTLSThroughGo(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d, _ := time.ParseDuration(r.URL.Query().Get("d"))
		time.Sleep(d)
		fmt.Fprint(w, "secure-ok")
	}))
	defer srv.Close()
	cafile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(cafile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o644); err != nil {
		t.Fatal(err)
	}
	httpsGet := func(target, extra string) (body *string, errText string) {
		var out struct {
			Body  *string `json:"body"`
			Error string  `json:"error"`
		}
		raw := get(t, "/https?url="+url.QueryEscape(target)+extra)
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		return out.Body, out.Error
	}

	if body, errText := httpsGet(srv.URL+"/?d=0s", "&cafile="+url.QueryEscape(cafile)); body == nil || *body != "secure-ok" {
		t.Fatalf("https with cafile: body %v, error %q", body, errText)
	}
	if body, errText := httpsGet(srv.URL+"/?d=0s", ""); body != nil || !strings.Contains(errText, "certificate") {
		t.Fatalf("an untrusted certificate was accepted: body %v, error %q", body, errText)
	}
	if body, errText := httpsGet(srv.URL+"/?d=0s", "&verify=0"); body == nil || *body != "secure-ok" {
		t.Fatalf("verify_peer=false: body %v, error %q", body, errText)
	}

	var stream struct{ Response, Error string }
	raw := get(t, "/tls-stream?addr="+strings.TrimPrefix(srv.URL, "https://")+"&cafile="+url.QueryEscape(cafile))
	if err := json.Unmarshal([]byte(raw), &stream); err != nil || stream.Response != "secure-ok" {
		t.Fatalf("tls:// stream: %s (%v)", raw, err)
	}

	const n = 50
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: n, MaxConnsPerHost: n}}
	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := client.Get("http://" + httpAddr + "/https?cafile=" + url.QueryEscape(cafile) + "&url=" + url.QueryEscape(srv.URL+"/?d=1s"))
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(b), `"body":"secure-ok"`) {
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
	t.Logf("%d https:// fetches of a 1s endpoint, on 1 PHP thread: %s", n, took.Round(time.Millisecond))
	if took > 3*time.Second {
		t.Fatalf("took %s; TLS blocked the thread", took)
	}
}

// PDO's constructor turns warnings into PDOException while it connects. When
// the connect suspends its fiber, other fibers on the thread must keep PHP's
// normal error handling.
func TestErrorModeStaysWithItsFiber(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { // accept, stay silent for a second, then hang up: PDO waits, then fails
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			time.AfterFunc(time.Second, func() { c.Close() })
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	done := make(chan string, 1)
	go func() { done <- get(t, fmt.Sprintf("/pdo-hang?port=%d", port)) }()
	time.Sleep(300 * time.Millisecond)
	if got := get(t, "/warning"); got != "warning" {
		t.Fatalf("a request running beside a connecting PDO got %q", got)
	}
	t.Logf("pdo: %s", <-done)
}

func TestUploadInFiberMode(t *testing.T) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("caption", "Nile")
	w, _ := mw.CreateFormFile("photo", "nile.jpg")
	_, _ = w.Write([]byte("JPEGDATA"))
	_ = mw.Close()
	resp, err := http.Post("http://"+httpAddr+"/upload", mw.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	want := `{"caption":"Nile","name":"nile.jpg","size":8,"contents":"JPEGDATA"}`
	if strings.TrimSpace(string(b)) != want {
		t.Fatalf("got %s, want %s", b, want)
	}
}
