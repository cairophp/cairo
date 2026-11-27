package test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// A new PDO pgsql connection runs statements in one round trip: nothing is
// prepared on the server, unless the app's options ask for it.
func TestPDOPgsqlOneRoundTripByDefault(t *testing.T) {
	pgsqlEnv(t)
	type reply struct {
		Answer           int  `json:"answer"`
		DisablePrepares  bool `json:"disable_prepares"`
		PreparedOnServer bool `json:"prepared_on_server"`
	}
	for _, c := range []struct {
		query string
		want  reply
	}{
		{"", reply{Answer: 42, DisablePrepares: true, PreparedOnServer: false}},
		{"?prepares=1", reply{Answer: 42, DisablePrepares: false, PreparedOnServer: true}},
	} {
		var got reply
		raw := get(t, "/pgsql/round-trips"+c.query)
		if err := json.Unmarshal([]byte(raw), &got); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if got != c.want {
			t.Errorf("/pgsql/round-trips%s = %+v, want %+v", c.query, got, c.want)
		}
	}
}

// A request cancelled in the middle of a query (its client went away) gives
// up on the query without waiting with its thread: the other requests on
// that one PHP thread go on at once.
func TestPDOPgsqlCancelledQueryLeavesTheThread(t *testing.T) {
	pgsqlEnv(t)
	client := &http.Client{Timeout: 200 * time.Millisecond}
	for range 5 {
		if _, err := client.Get("http://" + httpAddr + "/pgsql?seconds=2"); err == nil {
			t.Fatal("/pgsql answered before the client gave up")
		}
		start := time.Now()
		get(t, "/pgsql?seconds=0")
		if took := time.Since(start); took > 300*time.Millisecond {
			t.Fatalf("a query after a cancelled one took %s: the cancel held the thread", took)
		}
	}
}

// A deadline that passes in the middle of a query ends its wait at the
// deadline, and the connection's next query fails rather than read the
// answer of the query that was cut.
func TestPDOPgsqlDeadlineEndsTheQuery(t *testing.T) {
	pgsqlEnv(t)
	var got struct {
		Cut     string  `json:"cut"`
		Seconds float64 `json:"seconds"`
		Next    string  `json:"next"`
	}
	raw := get(t, "/pgsql/deadline")
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	if got.Cut != "deadline" || got.Seconds > 0.5 || got.Next != "failed" {
		t.Fatalf("/pgsql/deadline = %s, want the deadline at 0.2 s and the next query failed", raw)
	}
}
