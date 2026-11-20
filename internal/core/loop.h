#ifndef CAIRO_LOOP_H
#define CAIRO_LOOP_H

#include <stddef.h>
#include <stdint.h>

// The loop of a PHP thread: where it comes back between fibers and takes
// the events Go queued for it (loop.c).

// What a PHP thread shares with the Go code that sends it events, one per
// thread id. Go reads and writes it as atomics (mailbox in mailbox.go has
// the same layout), C with the __atomic builtins.
typedef struct {
	int64_t since;   // when the thread last came back to its loop, in unix ns; 0 while it sleeps
	int64_t stretch; // a moving average of how long it runs PHP between two visits, in ns
	// What the thread writes at every visit and what senders write share no
	// cache line, with each other or with another thread's mailbox.
	char pad[48];
	int32_t pending; // 1 while the thread's inbox, a Go channel, may hold an event
	char end[60];
} cairo_mailbox;

enum { cairo_mailbox_pending_at = offsetof(cairo_mailbox, pending) };

cairo_mailbox *cairo_mailbox_of(int tid);

// From here on the loop tells Go how long each stretch of PHP ran and
// fibers tell it of the waits they spend on the thread: an extension asked
// for a probe per request (probe.go).
void cairo_loop_probe(void);

// How long a thread watches its mailbox before it sleeps, in nanoseconds;
// 0, which it is until this is called, for not at all. Call it before the
// PHP threads start.
void cairo_loop_look(int64_t ns);

// Whether Go has an event for the calling PHP thread. It asks Go only when
// the mailbox says there may be one.
int cairo_loop_has_event(void);

#endif
