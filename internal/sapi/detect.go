package sapi

/*
#include "cairo.h"
#include "inbox.h"
*/
import "C"

import "time"

// detectBlocking is the development aid behind App::detectBlocking(): a
// request thread that runs PHP for longer than threshold without coming back
// for its next event is blocked, by a CPU loop or a blocking call Cairo does
// not hook, and every other fiber on it waits. The thread is asked to report:
// at its next safe point the running fiber logs the time and its stack
// (Cairo\Internal\reportBlocking). Once per busy stretch.
func (e *Engine) detectBlocking(threshold time.Duration) {
	every := max(threshold/4, 10*time.Millisecond)
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-e.shutdownCh:
			return
		case now := <-tick.C:
			for i := range e.workers {
				t := e.workers[i].Load()
				if t == nil {
					continue
				}
				since := int64(C.cairo_inbox_busy_since(C.int(t.id)))
				if since == 0 {
					continue
				}
				busy := now.Sub(time.Unix(0, since))
				taken := int64(C.cairo_inbox_taken(C.int(t.id)))
				if busy >= threshold && t.reportedAt.Swap(taken) != taken {
					C.cairo_report_blocking(C.int(t.id), C.int(busy.Milliseconds()))
				}
			}
		}
	}
}
