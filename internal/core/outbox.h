#ifndef CAIRO_OUTBOX_H
#define CAIRO_OUTBOX_H

#include "queue.h"

// What the PHP threads have for Go: the answers to the calls they took, that
// they have booted, and their own end. A thread queues its message without a call into Go, so it
// needs none of Go's processors for it, and one goroutine collects the
// messages of all threads (outbox.go). cairo_answer() in extension.h is what
// queues an answer.
enum { CAIRO_MSG_ANSWER = 1, CAIRO_MSG_EXIT = 2, CAIRO_MSG_READY = 3 };

// Opens the outbox. Returns a descriptor that becomes readable when a thread
// queues a message while Go waits for one (cairo_outbox_wait), or -1.
int cairo_outbox_open(void);

// All that is queued, oldest first, or NULL. The caller frees it.
cairo_msg *cairo_outbox_take(void);
void cairo_msg_free(cairo_msg *list);

// Go is about to wait on the descriptor. Returns 0 when a message came in
// the meantime, and Go is to take it instead.
int cairo_outbox_wait(void);

// PHP thread tid has run server.php and come to its loop for the first time:
// it takes requests from now on.
void cairo_outbox_ready(int tid);

// PHP thread tid has left PHP, with this exit status (id in the message).
// The thread sends it last, so Go sees it after the thread's answers.
void cairo_outbox_exit(int tid, int status);

#endif
