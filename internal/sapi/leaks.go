package sapi

import (
	"context"
	"fmt"
	"time"

	"github.com/cairophp/cairo/internal/core"
	"github.com/cairophp/cairo/internal/wire"
)

// App::detectLeaks(): php/leaks.php compares the app's static properties
// when each request ends and sends the ones that changed again. Go logs
// each static at most once a minute, like repeated queries, since a leaking
// static changes on every request.

var leaks repeatedLog

func init() {
	registerPHP("leak_watch.php")
	registerPHP("leaks.php", `Cairo\Internal\Leaks`)
	core.RegisterOp("leaks.report", func(_ context.Context, payload []byte) (any, error) {
		var req struct {
			Route   string   `json:"route"`
			Statics []string `json:"statics"`
		}
		if err := wire.Unmarshal(payload, &req); err != nil {
			return nil, fmt.Errorf("leaks.report: %w", err)
		}
		now := time.Now()
		for _, s := range req.Statics {
			if leaks.due(s, now) {
				core.Logger().Warn("static changed by a request", "route", req.Route, "static", s)
			}
		}
		return nil, nil
	})
}
