package sapi

import (
	"sync"
	"time"
)

// A warning that every request of a hot route would repeat is logged at most
// once a minute for each key, so it does not flood the log.

const (
	logOnceEvery = time.Minute
	logOnceMax   = 1000 // keys remembered; past it, all are forgotten
)

type logOnce struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// due reports whether key was not logged in the last logOnceEvery, and marks
// it logged.
func (l *logOnce) due(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if t, ok := l.last[key]; ok && now.Sub(t) < logOnceEvery {
		return false
	}
	if l.last == nil || len(l.last) >= logOnceMax {
		l.last = make(map[string]time.Time)
	}
	l.last[key] = now
	return true
}
