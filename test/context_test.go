package test

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func getJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	var out map[string]any
	raw := get(t, path)
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("%s: %s: %v", path, raw, err)
	}
	return out
}

func TestContextWithTimeout(t *testing.T) {
	out := getJSON(t, "/ctx/with-timeout")
	took, _ := out["took"].(float64)
	if out["step"] != "timed out" || took < 0.15 || took > 0.5 {
		t.Errorf("step under withTimeout(0.2): %v", out)
	}
	if out["child_err"] != `Cairo\Context\DeadlineExceeded` || out["parent_err"] != nil || out["child_remaining"] != float64(0) || out["nested"] != true {
		t.Errorf("contexts after the step: %v", out)
	}
}

func TestContextCancel(t *testing.T) {
	out := getJSON(t, "/ctx/cancel")
	waited, _ := out["waited"].(float64)
	sel, _ := out["select"].([]any)
	if len(sel) != 2 || sel[0] != "done" || sel[1] != false || waited < 0.05 || waited > 0.5 {
		t.Errorf("select on done() of a canceled context: %v", out)
	}
	if out["task"] != `Cairo\Context\Canceled` || out["err"] != `Cairo\Context\Canceled` || out["parent_err"] != nil || out["cancel_request"] != "refused" {
		t.Errorf("after cancel(): %v", out)
	}
}

func TestContextDoneAtDeadline(t *testing.T) {
	out := getJSON(t, "/ctx/done-deadline")
	waited, _ := out["waited"].(float64)
	sel, _ := out["select"].([]any)
	if len(sel) != 2 || sel[0] != "done" || waited < 0.1 || waited > 0.5 {
		t.Errorf("select on done() of withTimeout(0.15): %v", out)
	}
}

// A Go operation has a context only when PHP derived one for the code that
// asks: it ends at that context's deadline or when PHP cancels it (or what it
// derives from), does not start under one that has ended, and under a live
// one it runs to its end.
func TestContextEndsGoOperation(t *testing.T) {
	out := getJSON(t, "/ctx/go-operation?addr="+pongAfter(t))
	for _, tc := range []struct {
		key, how string
		min, max float64
	}{
		{"deadline", `Cairo\Context\DeadlineExceeded`, 0.1, 0.5},
		{"canceled", `Cairo\Context\Canceled`, 0.05, 0.5},
		{"ended", `Cairo\Context\Canceled`, 0, 0.05},
		{"parent_canceled", `Cairo\Context\Canceled`, 0.05, 0.5},
	} {
		got, _ := out[tc.key].([]any)
		if len(got) != 2 {
			t.Fatalf("%s: %v", tc.key, out)
		}
		if took, _ := got[1].(float64); got[0] != tc.how || took < tc.min || took > tc.max {
			t.Errorf("%s: a 0.6s operation ended as %v after %vs, want %s within %v to %vs", tc.key, got[0], got[1], tc.how, tc.min, tc.max)
		}
	}
	if out["request"] != "answered pong" || out["request_err"] != nil {
		t.Errorf("under a live context, with the request's own: %v, %v", out["request"], out["request_err"])
	}
}

// A task that its request did not await does not wait on after the answer:
// the request's context ends once the server is done with the request, and
// the task's waits end with it.
func TestContextEndsWithRequest(t *testing.T) {
	get(t, "/ctx/left-running")
	if out := getJSON(t, "/ctx/left-running/ended"); out["task"] != `Cairo\Context\Canceled` {
		t.Errorf("a 30s sleep in a task its request did not await: %v, want it canceled when the request was answered", out["task"])
	}
}

func TestContextValueFromGoMiddleware(t *testing.T) {
	req, _ := http.NewRequest("GET", "http://"+httpAddr+"/ctx/value", nil)
	req.Header.Set("X-Request-Id", "abc-123")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	if out["request_id"] != "abc-123" || out["header"] != "abc-123" || out["missing"] != "default" {
		t.Errorf("/ctx/value = %v", out)
	}
}
