package core

/*
#include "offload.h"
*/
import "C"

import "unsafe"

// An offload() job does not pass through Go: the request thread whose fiber
// asks queues it for an offload thread, and that thread queues the result
// for the request thread (offload.c). Go says which threads are the offload
// threads, and speaks for one that ended in the middle of a job.

// offloadPool tells C the offload threads, before any thread starts. They
// keep their ids: a request thread is replaced, an offload thread restarted.
func (e *Engine) offloadPool() error {
	tids := make([]C.int, len(e.offload))
	for i := range e.offload {
		tids[i] = C.int(e.offload[i].Load().id)
	}
	if C.cairo_offload_pool(unsafe.SliceData(tids), C.int(len(tids))) != 0 {
		return errNoMemory
	}
	return nil
}

// jobLost tells the fiber that waits for the job offload thread t had taken
// when it ended that the job has no result: nobody else will. The jobs
// still in its inbox are the next thread's.
func (e *Engine) jobLost(t *thread) {
	var from C.int
	id := int64(C.cairo_offload_lost(C.int(t.id), &from))
	if id == 0 {
		return
	}
	if w := e.own(from); w != nil {
		e.post(w, map[string]any{"type": "offloaded", "id": id, "error": "the offload thread stopped before the job was done"})
	}
}
