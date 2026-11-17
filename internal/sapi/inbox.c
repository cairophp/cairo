// A PHP thread's inbox: the events Go has for it (requests, finished I/O,
// shutdown), in order.
//
// The PHP thread waits for its next event here, in C, on a condition
// variable. It used to wait inside a Go callback on a channel; a goroutine
// called from a C thread is locked to it, so every wake-up went through the
// Go scheduler's locked-thread handoff (the waking thread hands over its P
// and stops) and the callback had to get a P back first. Now Go only calls
// cairo_inbox_push(), which copies the event and signals; the PHP thread
// wakes without the Go scheduler.
//
// The counters let Go keep its bookkeeping without a callback per event:
// which calls PHP has taken (supervise.go) and since when the thread has been
// busy (detect.go).

#include <pthread.h>
#include <stdatomic.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

#include "inbox.h"

// CAIRO_INBOX_CAP bounds the events waiting for one thread; a push beyond it
// fails and Go retries (see thread.push in engine.go).
#define CAIRO_INBOX_CAP 4096

typedef struct {
	pthread_mutex_t mu;
	pthread_cond_t cond;
	cairo_inbox_item *head, *tail;
	int count;
	int64_t pushed;               // events pushed, under mu
	_Atomic int64_t taken;        // events taken by PHP
	_Atomic int64_t calls_taken;  // of those, requests and offload jobs
	_Atomic int64_t busy_since;   // unix ns of the last take; 0 while waiting
} cairo_inbox;

static cairo_inbox inboxes[CAIRO_INBOX_THREADS];
static pthread_once_t inboxes_once = PTHREAD_ONCE_INIT;

static void inboxes_init(void)
{
	for (int i = 0; i < CAIRO_INBOX_THREADS; i++) {
		pthread_mutex_init(&inboxes[i].mu, NULL);
		pthread_cond_init(&inboxes[i].cond, NULL);
	}
}

static cairo_inbox *inbox(int tid)
{
	if (tid < 0 || tid >= CAIRO_INBOX_THREADS) {
		return NULL;
	}
	pthread_once(&inboxes_once, inboxes_init);
	return &inboxes[tid];
}

static int64_t now_ns(void)
{
	struct timespec ts;
	clock_gettime(CLOCK_REALTIME, &ts);
	return (int64_t)ts.tv_sec * 1000000000 + ts.tv_nsec;
}

int64_t cairo_inbox_push(int tid, const char *data, int len, int is_call)
{
	cairo_inbox *in = inbox(tid);
	if (in == NULL || len < 0) {
		return -2;
	}
	cairo_inbox_item *it = malloc(sizeof(*it) + (size_t)len);
	if (it == NULL) {
		return -2;
	}
	memcpy(it->data, data, (size_t)len);
	it->len = len;
	it->is_call = is_call;
	it->next = NULL;

	pthread_mutex_lock(&in->mu);
	if (in->count >= CAIRO_INBOX_CAP) {
		pthread_mutex_unlock(&in->mu);
		free(it);
		return -1;
	}
	if (in->tail != NULL) {
		in->tail->next = it;
	} else {
		in->head = it;
	}
	in->tail = it;
	in->count++;
	int64_t seq = ++in->pushed;
	pthread_cond_signal(&in->cond);
	pthread_mutex_unlock(&in->mu);
	return seq;
}

cairo_inbox_item *cairo_inbox_pop(int tid)
{
	cairo_inbox *in = inbox(tid);
	if (in == NULL) {
		return NULL;
	}
	atomic_store(&in->busy_since, 0);
	pthread_mutex_lock(&in->mu);
	while (in->head == NULL) {
		pthread_cond_wait(&in->cond, &in->mu);
	}
	cairo_inbox_item *it = in->head;
	in->head = it->next;
	if (in->head == NULL) {
		in->tail = NULL;
	}
	in->count--;
	pthread_mutex_unlock(&in->mu);
	atomic_fetch_add(&in->taken, 1);
	if (it->is_call) {
		atomic_fetch_add(&in->calls_taken, 1);
	}
	atomic_store(&in->busy_since, now_ns());
	return it;
}

int64_t cairo_inbox_taken(int tid)
{
	cairo_inbox *in = inbox(tid);
	return in == NULL ? 0 : atomic_load(&in->taken);
}

int64_t cairo_inbox_calls_taken(int tid)
{
	cairo_inbox *in = inbox(tid);
	return in == NULL ? 0 : atomic_load(&in->calls_taken);
}

int64_t cairo_inbox_busy_since(int tid)
{
	cairo_inbox *in = inbox(tid);
	return in == NULL ? 0 : atomic_load(&in->busy_since);
}

void cairo_inbox_reset(int tid)
{
	cairo_inbox *in = inbox(tid);
	if (in == NULL) {
		return;
	}
	pthread_mutex_lock(&in->mu);
	while (in->head != NULL) {
		cairo_inbox_item *it = in->head;
		in->head = it->next;
		free(it);
	}
	in->tail = NULL;
	in->count = 0;
	in->pushed = 0;
	atomic_store(&in->taken, 0);
	atomic_store(&in->calls_taken, 0);
	atomic_store(&in->busy_since, 0);
	pthread_mutex_unlock(&in->mu);
}
