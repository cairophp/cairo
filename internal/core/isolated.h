#ifndef CAIRO_ISOLATED_H
#define CAIRO_ISOLATED_H

#include <stddef.h>
#include <stdint.h>

#include "queue.h"

// The request threads of isolated mode (and classic mode) and the requests
// that wait for one, in one queue that the threads read themselves: a thread
// that answers its request takes the next one in the same call, without a
// trip through Go (isolated.go). The threads are counted by pool slot, as Go
// counts them: a slot is the place of one thread, which recycling and reloads
// hand from an old thread to its replacement.

// Before the PHP threads start: n slots, all free, and no thread in any; 0
// in fiber mode. Returns -1 when there is no memory for them.
int cairo_isolated_start(int n);

// Thread tid runs requests for slot (a spare thread is in the slot it is to
// take). -1 for a thread outside the request pool.
void cairo_isolated_place(int tid, int slot);

// A request for a free thread: returns the slot that is now the request's,
// for the caller to queue it to that slot's thread; or -1 when no slot is
// free and the request (n bytes and then tn more, copied) waits in the
// queue, until a thread takes it (goIsolatedTake); or -2 when there is no
// memory for it.
int cairo_isolated_dispatch(int64_t id, const char *data, size_t n, const char *tail, size_t tn);

// The request that held slot is over: the slot takes the next request that
// waits, or else is free. The calling thread answered it, or Go knows that
// the thread that held it ended.
void cairo_isolated_release(int slot);

// The calling thread tid answered a call: in the request pool, its slot is
// free for the next request.
void cairo_isolated_answered(int tid);

#endif
