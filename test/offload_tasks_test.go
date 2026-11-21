package test

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestOffloadTaskObjects(t *testing.T) {
	var out map[string]any
	if err := json.Unmarshal([]byte(get(t, "/offload/task?rounds=2000")), &out); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"same_as_local": true,
		"distinct":      true,
		"modes":         []any{"offload", "offload", "offload"},
		"error":         "DomainException: bad input",
		// The offload thread lives on: the cases below run on it.
		"unserializable": "Exception: Serialization of 'Closure' is not allowed",
	}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("/offload/task = %v, want %v", out, want)
	}
}

// Many jobs at once from the fibers of one request: every fiber gets the
// result of its own job, also when there are more than the queue of the one
// offload thread holds (4096) while it is busy with another.
func TestOffloadResultsReachTheirFibers(t *testing.T) {
	for _, path := range []string{"/offload/many?n=300", "/offload/many?n=4500&busy=0.2"} {
		out := getJSON(t, path)
		if out["jobs"] != out["right"] || out["right"] == 0.0 || out["busy"] != "done" {
			t.Errorf("%s = %v, want every job with its own result", path, out)
		}
	}
}

// The wait for a job ends with the context it is under, a job is not sent
// under one that has ended, and offload() works after both.
func TestOffloadWaitEndsWithItsContext(t *testing.T) {
	out := getJSON(t, "/offload/context")
	for _, tc := range []struct {
		key, how string
		min, max float64
	}{
		{"deadline", `Cairo\Context\DeadlineExceeded`, 0.1, 0.35},
		{"canceled", `Cairo\Context\Canceled`, 0.05, 0.35},
		{"ended", `Cairo\Context\Canceled`, 0, 0.05},
		{"live", "done", 0.35, 3},
	} {
		got, _ := out[tc.key].([]any)
		if len(got) != 2 {
			t.Fatalf("%s: %v", tc.key, out)
		}
		if took, _ := got[1].(float64); got[0] != tc.how || took < tc.min || took > tc.max {
			t.Errorf("%s: the wait for a job of 0.4s ended as %v after %vs, want %s within %v to %vs", tc.key, got[0], got[1], tc.how, tc.min, tc.max)
		}
	}
}
