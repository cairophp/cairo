package test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestChannels(t *testing.T) {
	var pipe map[string]any
	if err := json.Unmarshal([]byte(get(t, "/chan/pipeline")), &pipe); err != nil {
		t.Fatal(err)
	}
	if pipe["sum"] != float64(15) || pipe["buffered"] != float64(2) || pipe["count"] != float64(1) || pipe["first"] != float64(0) {
		t.Errorf("/chan/pipeline = %v", pipe)
	}

	var order []string
	if err := json.Unmarshal([]byte(get(t, "/chan/fanin")), &order); err != nil {
		t.Fatal(err)
	}
	if want := []string{"b=b", "c=c", "a=a"}; !reflect.DeepEqual(order, want) {
		t.Errorf("/chan/fanin = %v, want %v", order, want)
	}

	var to map[string]any
	if err := json.Unmarshal([]byte(get(t, "/chan/timeout")), &to); err != nil {
		t.Fatal(err)
	}
	if to["key"] != nil || to["now"] != nil || to["waited"].(float64) < 0.15 || to["waited"].(float64) > 0.5 {
		t.Errorf("/chan/timeout = %v", to)
	}

	var closed map[string]any
	if err := json.Unmarshal([]byte(get(t, "/chan/closed")), &closed); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"drained": "last", "recv": "channel closed", "send": "channel closed",
		"select": []any{"c", nil, false}, "waiting_sender": "closed",
	}
	if !reflect.DeepEqual(closed, want) {
		t.Errorf("/chan/closed = %v, want %v", closed, want)
	}
}

// Fibers waiting on channels suspend, so one PHP thread serves many at once.
func TestChannelHandoffDoesNotBlockThread(t *testing.T) {
	const n = 100
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := get(t, "/chan/handoff"); got != "ok" {
				t.Errorf("/chan/handoff = %q", got)
			}
		}()
	}
	wg.Wait()
	took := time.Since(start)
	if took > 3*time.Second {
		t.Fatalf("%d channel handoffs of 1s took %s on 1 PHP thread", n, took)
	}
	t.Logf("%d requests waiting 1s on a channel, on 1 PHP thread: %s", n, took.Round(time.Millisecond))
}

// A fiber waiting on a channel wakes when its request is canceled (the
// client went away): as the done() case of select(), or as Context\Canceled.
func TestChannelWaitEndsWithContext(t *testing.T) {
	client := &http.Client{Timeout: 300 * time.Millisecond}
	for _, path := range []string{"/chan/until-done", "/chan/wait-forever"} {
		if resp, err := client.Get("http://" + httpAddr + path); err == nil {
			resp.Body.Close()
			t.Fatalf("%s answered %d; want it to wait until the client gives up", path, resp.StatusCode)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	var seen map[string]string
	for time.Now().Before(deadline) {
		if err := json.Unmarshal([]byte(get(t, "/chan/seen")), &seen); err != nil {
			t.Fatal(err)
		}
		if len(seen) == 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	want := map[string]string{
		"until-done":   `done:closed:Cairo\Context\Canceled`,
		"wait-forever": `Cairo\Context\Canceled`,
	}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("after the clients left: %v, want %v", seen, want)
	}
}
