#ifndef CAIRO_INBOX_H
#define CAIRO_INBOX_H

#include <stdint.h>

// Matches CAIRO_MAX_THREADS in cairo.c and maxThreads in supervise.go.
#define CAIRO_INBOX_THREADS 4096

typedef struct cairo_inbox_item {
	struct cairo_inbox_item *next;
	int len;
	int is_call; // a request or offload job, which Go waits on
	char data[];
} cairo_inbox_item;

// Queues a copy of data for thread tid. Returns the event's sequence number
// (1, 2, ...), -1 when the inbox is full (retry later), or -2 on error.
int64_t cairo_inbox_push(int tid, const char *data, int len, int is_call);

// Waits for thread tid's next event; the caller frees it.
cairo_inbox_item *cairo_inbox_pop(int tid);

// Events taken so far; Go compares a call's sequence number with it.
int64_t cairo_inbox_taken(int tid);
// Requests and offload jobs taken so far.
int64_t cairo_inbox_calls_taken(int tid);
// Unix ns at which the thread took its current event; 0 while it waits.
int64_t cairo_inbox_busy_since(int tid);

// Empties the inbox and zeroes its counters, for a thread id put back to use.
void cairo_inbox_reset(int tid);

#endif
