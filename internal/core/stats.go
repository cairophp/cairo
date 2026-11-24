package core

import "sync/atomic"

// Stats is the state of the PHP threads at one moment, for an extension that
// reports it (metrics, readiness). The engine keeps these numbers for its
// own work; reading them costs a request nothing.
type Stats struct {
	// Threads is the request threads, and Busy how many of them are
	// running PHP right now.
	Threads, Busy int
	// OffloadThreads is the threads of Cairo\offload().
	OffloadThreads int
	// InFlight is the calls queued for the request threads or held by them
	// and not answered yet.
	InFlight int64
	// Restarts counts the threads started again after they stopped
	// unexpectedly, and Recycled those replaced at a limit of the app
	// (App::maxRequests(), maxMemory()), since the engine started.
	Restarts, Recycled int64
}

// counters is what Stats reports that no other state of the engine holds.
type counters struct {
	restarts, recycled atomic.Int64
}

// Stats reads the state of the PHP threads. It may be called from any
// goroutine, at any time after Start.
func (e *Engine) Stats() Stats {
	s := Stats{
		Restarts: e.counts.restarts.Load(),
		Recycled: e.counts.recycled.Load(),
	}
	for i := range e.workers {
		t := e.workers[i].Load()
		if t == nil {
			continue
		}
		s.Threads++
		s.InFlight += t.inflight.Load()
		if t.box.since.Load() != 0 {
			s.Busy++
		}
	}
	s.OffloadThreads = len(e.offload)
	return s
}
