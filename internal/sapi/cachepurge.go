package sapi

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cairophp/cairo/internal/cachetag"
)

// Cairo\Cache::purge(): drops the cached responses of routes with these tags.
func init() {
	registerOp("cache.purge", func(_ context.Context, payload []byte) (any, error) {
		var req struct {
			Tags []string `json:"tags"`
		}
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, fmt.Errorf("cache.purge: %w", err)
		}
		cachetag.Purge(req.Tags...)
		return nil, nil
	})
}
