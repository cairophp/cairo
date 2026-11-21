#ifndef CAIRO_QUEUE_H
#define CAIRO_QUEUE_H

#include <stddef.h>
#include <stdint.h>

// A queue between Go and a PHP thread, with any number of senders and one
// receiver, and no lock: a sender pushes its message onto a stack, and the
// receiver takes the whole stack at once and turns it around.

// A message is C memory that its sender allocates and its receiver frees.
typedef struct cairo_msg {
	struct cairo_msg *next;
	int64_t id;     // the call or the fiber it is about, 0 for none
	int32_t tid;    // to Go: the thread that sends it
	int32_t kind;   // to a thread: CAIRO_EVENT* (loop.h); to Go: CAIRO_MSG_* (outbox.h)
	uint32_t len;   // of the event, or of an answer's metadata
	uint32_t blen;  // of an answer's body, which follows its metadata in data
	char data[];
} cairo_msg;

static inline void cairo_queue_push(cairo_msg **queue, cairo_msg *m)
{
	m->next = __atomic_load_n(queue, __ATOMIC_RELAXED);
	while (!__atomic_compare_exchange_n(queue, &m->next, m, 1, __ATOMIC_SEQ_CST, __ATOMIC_RELAXED)) {
	}
}

// All that is queued, oldest first, or NULL.
static inline cairo_msg *cairo_queue_take(cairo_msg **queue)
{
	if (__atomic_load_n(queue, __ATOMIC_SEQ_CST) == NULL) {
		return NULL;   // the usual answer, without a write to the senders' memory
	}
	cairo_msg *m = __atomic_exchange_n(queue, NULL, __ATOMIC_SEQ_CST), *older = NULL;
	while (m != NULL) {
		cairo_msg *next = m->next;
		m->next = older;
		older = m;
		m = next;
	}
	return older;
}

#endif
