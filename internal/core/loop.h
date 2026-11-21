#ifndef CAIRO_LOOP_H
#define CAIRO_LOOP_H

#include "queue.h"

// The loop of a PHP thread: where it comes back between fibers, takes the
// events Go queued for it and, with none and no fiber to run, sleeps
// (loop.c).

// What a PHP thread shares with the Go code that sends it events, one per
// thread id. Go reads it as atomics (mailbox in mailbox.go has the same
// layout), C with the __atomic builtins.
typedef struct {
	int64_t since;    // when the thread last came back to its loop, in unix ns; 0 while it sleeps
	int64_t stretch;  // a moving average of how long it runs PHP between two visits, in ns
	int64_t took;     // how many events the thread has taken out of its inbox, and
	int64_t served;   // how many of them began a call
	cairo_msg *next;  // the events it took and has not read yet, oldest first
	int32_t handed;   // 1 when the thread gave Go an operation or an answer since its last visit
	// What the thread writes at every visit and what senders write share no
	// cache line, with each other or with another thread's mailbox.
	char pad[20];
	cairo_msg *inbox; // the events queued for the thread (queue.h)
	int64_t pushed;   // how many events were ever queued for it
	int32_t asleep;   // 1 while the thread sleeps: the sender that takes it off wakes the thread
	char end[44];
} cairo_mailbox;

enum { cairo_mailbox_inbox_at = offsetof(cairo_mailbox, inbox) };

cairo_mailbox *cairo_mailbox_of(int tid);

// From here on the loop tells Go how long each stretch of PHP ran and
// fibers tell it of the waits they spend on the thread: an extension asked
// for a probe per request (probe.go).
void cairo_loop_probe(void);

// How long a thread watches its inbox before it sleeps, in nanoseconds; 0,
// which it is until this is called, for not at all. Call it before the PHP
// threads start.
void cairo_loop_look(int64_t ns);

// Whether an event is queued for the calling PHP thread.
int cairo_loop_has_event(void);

// The calling PHP thread gave Go an answer: it looks before it sleeps, and
// when requests are probed, Go learns how long the thread ran up to here,
// before it learns of the answer.
void cairo_loop_answered(int tid);

// An event either begins a call (a request, an offload job) or does not.
enum { CAIRO_EVENT = 0, CAIRO_EVENT_CALL = 1 };

// Queues an event (internal/wire's format, n bytes, copied) for thread tid
// and wakes the thread when it sleeps. id is the call that the event begins,
// or the fiber it is for, or 0. Returns -1 when there is no memory for it.
int cairo_inbox_push(int tid, int kind, int64_t id, const char *data, size_t n);

// Takes what is queued for thread tid and it has not read out of its
// mailbox, oldest first: for a thread that has ended. The caller frees it
// (cairo_msg_free in outbox.h).
cairo_msg *cairo_inbox_drain(int tid);

#endif
