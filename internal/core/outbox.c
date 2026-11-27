// What the PHP threads have for Go. See outbox.h.

#include <errno.h>
#include <stdlib.h>
#include <string.h>
#include <sys/eventfd.h>
#include <unistd.h>

#include "extension.h"
#include "isolated.h"
#include "loop.h"
#include "outbox.h"

static cairo_msg *outbox;
static int32_t waited;   // 1 while the collecting goroutine waits on fd
static int fd = -1;

int cairo_outbox_open(void)
{
	fd = eventfd(0, EFD_NONBLOCK | EFD_CLOEXEC);
	return fd;
}

// A thread queues its message and then reads waited; Go writes waited and
// then reads the queue: one of the two sees the other.
static void push(cairo_msg *m)
{
	cairo_queue_push(&outbox, m);
	if (__atomic_load_n(&waited, __ATOMIC_SEQ_CST) != 0 && __atomic_exchange_n(&waited, 0, __ATOMIC_SEQ_CST) != 0) {
		uint64_t one = 1;
		while (write(fd, &one, sizeof(one)) < 0 && errno == EINTR) {
		}
	}
}

cairo_msg *cairo_outbox_take(void)
{
	return cairo_queue_take(&outbox);
}

int cairo_outbox_wait(void)
{
	__atomic_store_n(&waited, 1, __ATOMIC_SEQ_CST);
	if (__atomic_load_n(&outbox, __ATOMIC_SEQ_CST) == NULL) {
		return 1;
	}
	__atomic_store_n(&waited, 0, __ATOMIC_SEQ_CST);
	return 0;
}

void cairo_msg_free(cairo_msg *list)
{
	while (list != NULL) {
		cairo_msg *next = list->next;
		free(list);
		list = next;
	}
}

int cairo_answer(int64_t id, const char *meta, size_t n, const char *body, size_t bn)
{
	int tid = cairo_thread_id();
	if (tid < 0) {
		return 0;   // the declare phase takes no calls
	}
	cairo_msg *m = n > INT32_MAX || bn > INT32_MAX ? NULL : malloc(sizeof(*m) + n + bn);
	if (m == NULL) {
		return -1;
	}
	m->id = id;
	m->tid = tid;
	m->kind = CAIRO_MSG_ANSWER;
	m->len = (uint32_t)n;
	m->blen = (uint32_t)bn;
	memcpy(m->data, meta, n);
	if (bn > 0) {
		memcpy(m->data + n, body, bn);
	}
	cairo_loop_answered(tid);
	push(m);
	cairo_isolated_answered(tid);   // after the answer: the next request does not go before it
	return 0;
}

// A message of the thread about itself.
static void tell(int tid, int kind, int64_t id)
{
	cairo_msg *m = malloc(sizeof(*m));
	if (m == NULL) {
		abort();   // Go would wait for this thread for ever
	}
	m->id = id;
	m->tid = tid;
	m->kind = kind;
	m->len = m->blen = 0;
	push(m);
}

void cairo_outbox_ready(int tid)
{
	if (fd < 0) {
		return;   // the command line: Go collects nothing
	}
	tell(tid, CAIRO_MSG_READY, 0);
}

void cairo_outbox_exit(int tid, int status)
{
	tell(tid, CAIRO_MSG_EXIT, status);
}
