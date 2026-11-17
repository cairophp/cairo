package test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// A request cancelled while it holds a pooled connection (the client goes
// away mid-wait) drops the connection, through with() and through get(): a
// cancelled wait may have cut a reply short, so it is never handed out again.
func TestPoolDropsConnectionOfCancelledRequest(t *testing.T) {
	client := &http.Client{Timeout: 200 * time.Millisecond}
	var stats struct {
		Opened int `json:"opened"`
		Open   int `json:"open"`
		Idle   int `json:"idle"`
	}
	for i, mode := range []string{"with", "get"} {
		if _, err := client.Get("http://" + httpAddr + "/pool/hold?mode=" + mode); err == nil {
			t.Fatalf("%s: /pool/hold answered before the client gave up", mode)
		}
		deadline := time.Now().Add(3 * time.Second)
		for {
			if err := json.Unmarshal([]byte(get(t, "/pool/hold-stats")), &stats); err != nil {
				t.Fatal(err)
			}
			if stats.Open == 0 || time.Now().After(deadline) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if stats.Opened != i+1 || stats.Open != 0 || stats.Idle != 0 {
			t.Fatalf("%s: after the cancelled request, pool = %+v; want the connection dropped", mode, stats)
		}
	}
}
