package sapi

/*
#include "pdo_watch.h"
*/
import "C"

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/cairophp/cairo/internal/core"
	"github.com/cairophp/cairo/internal/wire"
)

// App::repeatedQueries(): php/queries.php counts each request's statements
// by shape and, when the request ends, sends the ones that ran too often.
// Go logs them, each route and statement at most once a minute, so a hot
// route with an N+1 does not flood the log.

const (
	repeatedEvery = time.Minute
	repeatedMax   = 1000 // routes and statements remembered; past it, all are forgotten
)

type repeatedLog struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// due reports whether key was not logged in the last repeatedEvery, and
// marks it logged.
func (l *repeatedLog) due(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if t, ok := l.last[key]; ok && now.Sub(t) < repeatedEvery {
		return false
	}
	if l.last == nil || len(l.last) >= repeatedMax {
		l.last = make(map[string]time.Time)
	}
	l.last[key] = now
	return true
}

var repeated repeatedLog

func init() {
	registerPHP("query_watch.php")
	registerPHP("queries.php", `Cairo\Internal\Queries`)
	registerC(&C.cairo_pdo_watch)
	core.RegisterOp("sql.repeated", func(_ context.Context, payload []byte) (any, error) {
		var req struct {
			Route   string `json:"route"`
			Queries []struct {
				Query string `json:"query"`
				Count int    `json:"count"`
			} `json:"queries"`
		}
		if err := wire.Unmarshal(payload, &req); err != nil {
			return nil, fmt.Errorf("sql.repeated: %w", err)
		}
		now := time.Now()
		for _, q := range req.Queries {
			if repeated.due(req.Route+"\x00"+q.Query, now) {
				core.Logger().Warn("repeated query", "route", req.Route, "count", q.Count, "query", q.Query)
			}
		}
		return nil, nil
	})
}
