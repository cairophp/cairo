package test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// runWebSocket runs examples/websocket on one PHP thread and returns its
// address, with what stops it as a deploy does.
func runWebSocket(t *testing.T, env ...string) (addr string, stop func()) {
	t.Helper()
	addr = freeAddr()
	logs := &syncBuffer{}
	cmd := exec.Command(cairoBin, "serve", "../examples/websocket/server.php", "--threads", "1", "--offload-threads", "1")
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
	return addr, stop
}

// openSocket opens a WebSocket of the example; the test ends it.
func openSocket(t *testing.T, addr, path string, opts *websocket.DialOptions) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws://"+addr+path, opts)
	if err != nil {
		t.Fatalf("dial %s: %v", path, err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

// next is the next text message of conn, or how the socket ended.
func next(conn *websocket.Conn) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	return string(data), err
}

func want(t *testing.T, conn *websocket.Conn, message string) {
	t.Helper()
	if got, err := next(conn); err != nil || got != message {
		t.Fatalf("message = %q (%v), want %q", got, err, message)
	}
}

// What the example noted under key, once it has.
func noted(t *testing.T, addr, key string) string {
	t.Helper()
	return seen(t, "http://"+addr, key)
}

// A message comes back as it was sent, text or binary, and one that is
// larger than the handler allows closes the socket.
func TestWebSocketEcho(t *testing.T) {
	addr, _ := runWebSocket(t)
	conn := openSocket(t, addr, "/echo?max=1000", nil)
	ctx := context.Background()
	for _, tc := range []struct {
		typ  websocket.MessageType
		data string
	}{
		{websocket.MessageText, "hello, مرحبا"},
		{websocket.MessageBinary, "\x00\x01\xff"},
		{websocket.MessageText, ""},
		{websocket.MessageBinary, strings.Repeat("x", 1000)},
	} {
		if err := conn.Write(ctx, tc.typ, []byte(tc.data)); err != nil {
			t.Fatal(err)
		}
		typ, data, err := conn.Read(ctx)
		if err != nil || typ != tc.typ || string(data) != tc.data {
			t.Fatalf("echo of %d bytes of type %v: %d bytes of type %v (%v)", len(tc.data), tc.typ, len(data), typ, err)
		}
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte(strings.Repeat("x", 1001))); err != nil {
		t.Fatal(err)
	}
	if _, err := next(conn); websocket.CloseStatus(err) != websocket.StatusMessageTooBig {
		t.Errorf("after a message over the limit: %v, want the status %d", err, websocket.StatusMessageTooBig)
	}

	// A client that closes ends receive() with null, and the handler returns.
	conn = openSocket(t, addr, "/echo", nil)
	if err := conn.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Errorf("close: %v", err)
	}
	if note := noted(t, addr, "echo"); note != "the client closed" {
		t.Errorf("note = %q", note)
	}
}

// Fifty sockets whose handlers each wait a second share one PHP thread:
// each is a fiber.
func TestWebSocketsShareOneThread(t *testing.T) {
	addr, _ := runWebSocket(t)
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, _, err := websocket.Dial(context.Background(), "ws://"+addr+"/late", nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.CloseNow()
			if got, err := next(conn); err != nil || got != "late" {
				t.Errorf("message = %q (%v)", got, err)
			}
		}()
	}
	wg.Wait()
	if took := time.Since(start); took > 2500*time.Millisecond {
		t.Errorf("50 sockets that wait 1 s took %s on one PHP thread", took)
	}
}

// The route's handler decides before there is a socket, a page of another
// origin is refused, and the client chooses among the server's subprotocols.
func TestWebSocketHandshake(t *testing.T) {
	addr, _ := runWebSocket(t)
	ctx := context.Background()
	refused := func(path string, header http.Header, status int) {
		t.Helper()
		conn, resp, err := websocket.Dial(ctx, "ws://"+addr+path, &websocket.DialOptions{HTTPHeader: header})
		if err == nil {
			conn.CloseNow()
			t.Errorf("%s with %v: connected, want status %d", path, header, status)
		} else if resp == nil || resp.StatusCode != status {
			t.Errorf("%s with %v: %v, want status %d", path, header, err, status)
		}
	}

	refused("/members", nil, http.StatusUnauthorized)
	conn := openSocket(t, addr, "/members", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer let-me-in"}}})
	want(t, conn, "welcome")
	if _, err := next(conn); websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Errorf("after the handler returned: %v, want a normal close", err)
	}

	refused("/partners", http.Header{"Origin": {"https://evil.example"}}, http.StatusForbidden)
	for _, origin := range []string{"https://app.partner.example", "http://" + addr, ""} {
		header := http.Header{}
		if origin != "" {
			header.Set("Origin", origin)
		}
		want(t, openSocket(t, addr, "/partners", &websocket.DialOptions{HTTPHeader: header}), "hello, partner")
	}

	conn = openSocket(t, addr, "/protocols", &websocket.DialOptions{Subprotocols: []string{"chat.v1", "other"}})
	want(t, conn, "speaking chat.v1")
	want(t, openSocket(t, addr, "/protocols", nil), "speaking ")

	// A request that asks for no WebSocket is told what the address is for.
	resp, err := http.Get("http://" + addr + "/echo")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Errorf("a plain GET of a WebSocket's address: status %d, want %d", resp.StatusCode, http.StatusUpgradeRequired)
	}
}

// The server closes with a status of its own, a handler that fails closes
// with the status of a server's error, and both sides see the end.
func TestWebSocketEnds(t *testing.T) {
	addr, _ := runWebSocket(t)
	conn := openSocket(t, addr, "/bye", nil)
	want(t, conn, "last words")
	_, err := next(conn)
	var closed websocket.CloseError
	if !asCloseError(err, &closed) || closed.Code != 4001 || closed.Reason != "that was all" {
		t.Errorf("after close(4001): %v", err)
	}
	if note := noted(t, addr, "bye"); note != "closed" {
		t.Errorf("receive() after close(): %q", note)
	}

	conn = openSocket(t, addr, "/fails", nil)
	want(t, conn, "so far so good")
	if _, err := next(conn); websocket.CloseStatus(err) != websocket.StatusInternalError {
		t.Errorf("after the handler failed: %v, want the status %d", err, websocket.StatusInternalError)
	}
}

func asCloseError(err error, target *websocket.CloseError) bool {
	if websocket.CloseStatus(err) == -1 {
		return false
	}
	*target = websocket.CloseError{Code: websocket.CloseStatus(err)}
	if i := strings.Index(err.Error(), `reason = "`); i >= 0 {
		target.Reason = strings.TrimSuffix(err.Error()[i+len(`reason = "`):], `"`)
	}
	return true
}

// A handler that only sends learns that its client left, wherever it
// waits; one that receives gets what the client sent before it left.
func TestWebSocketClientLeaves(t *testing.T) {
	addr, _ := runWebSocket(t)
	conn := openSocket(t, addr, "/ticks", nil)
	want(t, conn, "tick 1")
	_ = conn.CloseNow()
	if note := noted(t, addr, "ticks"); note != "the client left" {
		t.Errorf("note = %q", note)
	}

	conn = openSocket(t, addr, "/count", nil)
	for _, m := range []string{"a", "b", "c"} {
		if err := conn.Write(context.Background(), websocket.MessageText, []byte(m)); err != nil {
			t.Fatal(err)
		}
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
	if note := noted(t, addr, "count"); note != "a,b,c" {
		t.Errorf("received = %q, want all three", note)
	}
}

// A client that does not answer the server's pings is dropped.
func TestWebSocketPing(t *testing.T) {
	addr, _ := runWebSocket(t, "CAIRO_PING=0.2")
	conn := openSocket(t, addr, "/ticks", nil)
	want(t, conn, "tick 1")
	// From here the client reads nothing, so it answers no ping.
	if note := noted(t, addr, "ticks"); note != "the client left" {
		t.Errorf("note = %q", note)
	}
}

// publish posts data to a topic of the example and returns how many listen.
func publish(t *testing.T, addr, topic, query, data string) int {
	t.Helper()
	resp, err := http.Post("http://"+addr+"/publish/"+topic+"?"+query, "text/plain", strings.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got struct{ Listening int }
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("publish to %s: status %d, %v", topic, resp.StatusCode, err)
	}
	return got.Listening
}

// listen opens the Server-Sent Events of the example's /news and returns
// what reads its events, each as the lines of one. The events by which a
// test counts the listeners (listeners) are left out.
func listen(t *testing.T, addr, query string) func() string {
	t.Helper()
	resp, err := http.Get("http://" + addr + "/news?token=reader&" + query)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("/news: status %d, type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	lines := bufio.NewReader(resp.Body)
	return func() string {
		var event []string
		for {
			line, err := lines.ReadString('\n')
			if err != nil {
				return strings.Join(event, "|") + " (" + err.Error() + ")"
			}
			if line = strings.TrimSuffix(line, "\n"); line != "" {
				event = append(event, line)
				continue
			}
			if len(event) != 1 || event[0] != "data: "+counting {
				return strings.Join(event, "|")
			}
			event = event[:0]
		}
	}
}

// What one client says in a room, every member hears, sent by Go: the
// fibers only read. A member that leaves is announced by what it deferred.
func TestTopicsBetweenWebSockets(t *testing.T) {
	addr, _ := runWebSocket(t)
	amira := openSocket(t, addr, "/chat?name=amira", nil)
	want(t, amira, "amira joined")
	omar := openSocket(t, addr, "/chat?name=omar", nil)
	want(t, amira, "omar joined")
	want(t, omar, "omar joined")

	if err := amira.Write(context.Background(), websocket.MessageText, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	want(t, amira, "amira: hello")
	want(t, omar, "amira: hello")

	if n := publish(t, addr, "room", "", "from a plain request"); n != 2 {
		t.Errorf("listening = %d, want 2", n)
	}
	want(t, amira, "from a plain request")
	want(t, omar, "from a plain request")

	_ = omar.Close(websocket.StatusNormalClosure, "")
	want(t, amira, "omar left")
}

// Server-Sent Events from topics: PHP says who listens to what and Go
// serves the events. A client hears its own topics only, and holds no PHP
// memory while it listens.
func TestTopicsAsServerSentEvents(t *testing.T) {
	addr, _ := runWebSocket(t)
	resp, err := http.Get("http://" + addr + "/news")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("/news without a token: status %d, want 403", resp.StatusCode)
	}

	memory := func() int {
		n, _ := strconv.Atoi(fetchBody(t, "http://"+addr+"/memory"))
		return n
	}
	memory() // the first request of the thread
	before := memory()
	mona, hany := listen(t, addr, "user=mona"), listen(t, addr, "user=hany")
	for i := 0; i < 98; i++ {
		listen(t, addr, "user=crowd")
	}
	waitUntil(t, "all listen to news", func() bool { return listeners(t, addr, "news") == 100 })
	if n := listeners(t, addr, "nobody"); n != 0 {
		t.Errorf("listening to a topic that nobody named = %d", n)
	}
	if grew := memory() - before; grew > 64<<10 {
		t.Errorf("100 listeners hold %d bytes of PHP memory, want none of it", grew)
	}

	if n := publish(t, addr, "news", "event=headline&id=7", "two\nlines"); n != 100 {
		t.Errorf("listening to news = %d, want 100", n)
	}
	want := "event: headline|id: 7|data: two|data: lines"
	if got := mona(); got != want {
		t.Errorf("mona's event = %q, want %q", got, want)
	}
	if got := hany(); got != want {
		t.Errorf("hany's event = %q, want %q", got, want)
	}
	if n := publish(t, addr, "user.mona", "", "for you"); n != 1 {
		t.Errorf("listening to user.mona = %d, want 1", n)
	}
	publish(t, addr, "news", "", "for all")
	if got := mona(); got != "data: for you" {
		t.Errorf("mona's event = %q", got)
	}
	if got := hany(); got != "data: for all" {
		t.Errorf("hany's event = %q: it is not mona's", got)
	}
}

// listeners is how many listen to a topic, which publishing there tells.
func listeners(t *testing.T, addr, topic string) int {
	t.Helper()
	return publish(t, addr, topic, "", counting)
}

const counting = "only counting"

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("never: %s", what)
}

// A quiet connection is kept open by comments, which are no events.
func TestTopicsKeepAlive(t *testing.T) {
	addr, _ := runWebSocket(t)
	resp, err := http.Get("http://" + addr + "/news?token=reader&keep=0.05")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != ": \n" {
		t.Errorf("first line of a quiet stream = %q (%v), want a comment", line, err)
	}
}

// A client that does not read is dropped once its queue is full, and the
// others lose nothing by it.
func TestTopicsDropSlowClient(t *testing.T) {
	addr, _ := runWebSocket(t)
	slow, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()
	fmt.Fprintf(slow, "GET /news?token=reader HTTP/1.1\r\nHost: %s\r\n\r\n", addr)
	reader := listen(t, addr, "")
	waitUntil(t, "both listen", func() bool { return listeners(t, addr, "news") == 2 })

	big := strings.Repeat("x", 256<<10)
	for i := 0; i < 200; i++ {
		publish(t, addr, "news", "id="+strconv.Itoa(i), big)
		if got := reader(); got != "id: "+strconv.Itoa(i)+"|data: "+big {
			t.Fatalf("event %d of the client that reads: %.40q", i, got)
		}
	}
	waitUntil(t, "the slow client is dropped", func() bool { return listeners(t, addr, "news") == 1 })
}

// In isolated mode a WebSocket holds its PHP thread, as a request does, but
// the events that Go serves hold none: one thread has twenty listeners and
// still answers.
func TestTopicsInIsolatedMode(t *testing.T) {
	addr, _ := runWebSocket(t, "CAIRO_CONCURRENCY=isolated")
	readers := make([]func() string, 20)
	for i := range readers {
		readers[i] = listen(t, addr, "")
	}
	waitUntil(t, "all listen", func() bool { return listeners(t, addr, "news") == len(readers) })
	publish(t, addr, "news", "", "to all")
	for i, read := range readers {
		if got := read(); got != "data: to all" {
			t.Fatalf("listener %d got %q", i, got)
		}
	}
	conn := openSocket(t, addr, "/echo", nil)
	if err := conn.Write(context.Background(), websocket.MessageText, []byte("isolated")); err != nil {
		t.Fatal(err)
	}
	want(t, conn, "isolated")
}

// A server that stops tells its WebSockets to go away and ends the events
// that it serves, instead of waiting for them.
func TestWebSocketsAndTopicsWhenTheServerStops(t *testing.T) {
	addr, stop := runWebSocket(t)
	conn := openSocket(t, addr, "/echo", nil)
	if err := conn.Write(context.Background(), websocket.MessageText, []byte("still there?")); err != nil {
		t.Fatal(err)
	}
	want(t, conn, "still there?")
	resp, err := http.Get("http://" + addr + "/news?token=reader")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	waitUntil(t, "the client listens", func() bool { return listeners(t, addr, "news") == 1 })

	start := time.Now()
	left := make(chan error, 1)
	go func() {
		_, err := next(conn)
		left <- err
	}()
	stop()
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the server took %s to stop with a socket and a listener open", took)
	}
	if err := <-left; websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Errorf("the socket of a stopping server: %v, want the status %d", err, websocket.StatusGoingAway)
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Errorf("the events of a stopped server: %v, want a clean end", err)
	}
}
