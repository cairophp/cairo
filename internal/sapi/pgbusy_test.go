package sapi

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// A handle of a shared pool that one fiber at a time uses is never "busy":
// the give-back of the request that used it before holds the handle for a
// moment, and a statement waits for it. The request kind is one that needs
// no server.
func TestPgHandleIsNotBusyDuringGiveBack(t *testing.T) {
	pc := &pgConn{shared: &pgShared{}, notify: &pgNotifyQueue{}}
	id := pgNextID.Add(1)
	pgConns.Store(id, pc)
	t.Cleanup(func() { pgConns.Delete(id) })

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				pgDetachIf(pc, -1) // the give-back of a request that no longer has the handle
			}
		}
	}()
	for i := range 200_000 {
		resp := pgDo(context.Background(), pgRequest{Kind: "notify", Conn: id})
		if msg, _ := resp["error"].(string); strings.Contains(msg, "busy") {
			t.Errorf("a statement beside a give-back, at %d: %s", i, msg)
			break
		}
	}
	close(stop)
	wg.Wait()
}
