// The request threads of isolated mode and the queue of requests that wait
// for one. See isolated.h.

#include <pthread.h>
#include <stdbool.h>
#include <stdlib.h>
#include <string.h>

#include "extension.h"
#include "isolated.h"
#include "loop.h"
#include "outbox.h"
#include "_cgo_export.h"

// All under mu. A slot is busy from the moment a request is given to it
// until that request is over; a free slot is in the ring, oldest first, and
// is never free while a request waits.
static pthread_mutex_t mu = PTHREAD_MUTEX_INITIALIZER;
static cairo_msg *head, **tailp = &head;  // the requests that wait, oldest first
static int *ring;                         // the free slots, first in, first out
static int nslots, first, nfree;
static bool *busy;
// Written before the thread runs (Go starts it after), read by the thread.
static int slot_of[CAIRO_MAX_THREADS];

int cairo_isolated_start(int n)
{
	pthread_mutex_lock(&mu);
	cairo_msg_free(head);   // of an engine before this one in the process
	head = NULL;
	tailp = &head;
	free(ring);
	free(busy);
	ring = n > 0 ? calloc((size_t)n, sizeof(*ring)) : NULL;
	busy = n > 0 ? calloc((size_t)n, sizeof(*busy)) : NULL;
	int failed = n > 0 && (ring == NULL || busy == NULL);
	nslots = failed ? 0 : n;
	first = 0;
	nfree = nslots;
	for (int i = 0; i < nslots; i++) {
		ring[i] = i;
	}
	pthread_mutex_unlock(&mu);
	for (int i = 0; i < CAIRO_MAX_THREADS; i++) {
		slot_of[i] = -1;
	}
	return failed ? -1 : 0;
}

void cairo_isolated_place(int tid, int slot)
{
	slot_of[tid] = slot;
}

// Takes the free slot that has been free the longest. With mu held.
static int take_slot(void)
{
	int s = ring[first];
	first = (first + 1) % nslots;
	nfree--;
	busy[s] = true;
	return s;
}

int cairo_isolated_dispatch(int64_t id, const char *data, size_t n, const char *tail, size_t tn)
{
	pthread_mutex_lock(&mu);
	if (nfree > 0) {
		int s = take_slot();
		pthread_mutex_unlock(&mu);
		return s;
	}
	pthread_mutex_unlock(&mu);
	// Copied outside the lock; the thread that takes it frees it.
	cairo_msg *m = n + tn > INT32_MAX ? NULL : malloc(sizeof(*m) + n + tn);
	if (m == NULL) {
		return -2;
	}
	m->next = NULL;
	m->id = id;
	m->kind = CAIRO_EVENT_CALL;
	m->len = (uint32_t)(n + tn);
	m->blen = 0;
	memcpy(m->data, data, n);
	if (tn > 0) {
		memcpy(m->data + n, tail, tn);
	}
	pthread_mutex_lock(&mu);
	if (nfree > 0) {   // a slot came free meanwhile
		int s = take_slot();
		pthread_mutex_unlock(&mu);
		free(m);
		return s;
	}
	*tailp = m;
	tailp = &m->next;
	pthread_mutex_unlock(&mu);
	return -1;
}

void cairo_isolated_release(int slot)
{
	if (slot < 0 || slot >= nslots) {
		return;
	}
	pthread_mutex_lock(&mu);
	if (!busy[slot]) {
		pthread_mutex_unlock(&mu);
		return;   // already over: a slot is in the ring once at most
	}
	for (;;) {
		cairo_msg *m = head;
		if (m == NULL) {
			busy[slot] = false;
			ring[(first + nfree) % nslots] = slot;
			nfree++;
			break;
		}
		head = m->next;
		if (head == NULL) {
			tailp = &head;
		}
		m->next = NULL;
		// The slot stays busy while Go is asked, so nothing else takes it.
		pthread_mutex_unlock(&mu);
		if (goIsolatedTake(slot, m->id, m)) {
			return;
		}
		free(m);   // its request ended while it waited
		pthread_mutex_lock(&mu);
	}
	pthread_mutex_unlock(&mu);
}

void cairo_isolated_answered(int tid)
{
	if (tid >= 0 && tid < CAIRO_MAX_THREADS && slot_of[tid] >= 0) {
		cairo_isolated_release(slot_of[tid]);
	}
}
