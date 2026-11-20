package core

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/cairophp/cairo/internal/wire"
)

// runOffload sends a job to the offload pool and waits for its result.
func (e *Engine) runOffload(ctx context.Context, payload string) (string, error) {
	if len(e.offload) == 0 {
		return "", errors.New("no offload threads")
	}
	t := acquire(e.offload)
	c := &call{thread: t, resp: make(chan *reply, 1)}
	id := e.nextID.Add(1)
	e.calls.Store(id, c)
	t.inflight.Add(1)
	b := wire.Encode(map[string]any{"type": "offload", "id": id, "payload": payload})
	select {
	case t.inbox <- event{data: b, call: c}:
		t.sendMu.RUnlock()
	case <-ctx.Done():
		t.sendMu.RUnlock()
		if _, ok := e.calls.LoadAndDelete(id); ok {
			t.inflight.Add(-1)
		}
		return "", ctx.Err()
	}
	select {
	case r := <-c.resp:
		if r == nil {
			return "", ErrThreadStopped
		}
		var out struct {
			Payload string `json:"payload"`
		}
		if err := json.Unmarshal(r.meta, &out); err != nil {
			return "", err
		}
		return out.Payload, nil
	case <-ctx.Done():
		// The job keeps running on its thread; its answer is dropped.
		return "", ctx.Err()
	}
}
