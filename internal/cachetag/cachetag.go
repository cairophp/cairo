// Package cachetag holds the tags of cached responses. A route's ->cache()
// names its tags, and Cairo\Cache::purge() bumps a tag's generation: the
// response cache treats an entry stored under an older generation as gone.
// It is its own package because the cache lives in httpserver and the purge
// arrives through sapi, which do not import each other.
package cachetag

import (
	"sync"
	"sync/atomic"
)

var tags sync.Map // tag name -> *atomic.Uint64, its generation

// Counter returns the generation counter of a tag a route declared.
func Counter(name string) *atomic.Uint64 {
	c, _ := tags.LoadOrStore(name, new(atomic.Uint64))
	return c.(*atomic.Uint64)
}

// Purge drops every cached response tagged with one of names. A tag no route
// declared is ignored, so untrusted names cannot grow the table.
func Purge(names ...string) {
	for _, name := range names {
		if c, ok := tags.Load(name); ok {
			c.(*atomic.Uint64).Add(1)
		}
	}
}
