// The loop of a PHP thread, where it comes back between fibers: it takes the
// events Go queued for it and the sockets that are ready for its fibers and,
// with none and no fiber to run, sleeps, all without a call into Go. See
// loop.h.

#include <linux/futex.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <sys/syscall.h>
#include <time.h>
#include <unistd.h>

#include <php.h>
#include <zend_exceptions.h>

#include "extension.h"
#include "loop.h"
#include "offload.h"
#include "sockets.h"
#include "wire.h"
#include "_cgo_export.h"

static _Alignas(64) cairo_mailbox boxes[CAIRO_MAX_THREADS];
static int32_t probed;
static int64_t look_ns;   // written once, before the PHP threads start

cairo_mailbox *cairo_mailbox_of(int tid)
{
	return &boxes[tid];
}

void cairo_loop_probe(void)
{
	__atomic_store_n(&probed, 1, __ATOMIC_SEQ_CST);
}

void cairo_loop_look(int64_t ns)
{
	look_ns = ns;
}

static long futex(int32_t *word, int op, int32_t val, const struct timespec *timeout)
{
	return syscall(SYS_futex, word, op, val, timeout, NULL, 0);
}

int cairo_inbox_push(int tid, int kind, int64_t id, const char *data, size_t n)
{
	cairo_msg *m = n > INT32_MAX ? NULL : malloc(sizeof(*m) + n);
	if (m == NULL) {
		return -1;
	}
	m->id = id;
	m->kind = kind;
	m->len = (uint32_t)n;
	m->blen = 0;
	memcpy(m->data, data, n);
	cairo_inbox_queue(tid, m);
	return 0;
}

void cairo_inbox_queue(int tid, cairo_msg *m)
{
	cairo_mailbox *box = &boxes[tid];
	m->tid = tid;
	cairo_queue_push(&box->inbox, m);
	__atomic_fetch_add(&box->pushed, 1, __ATOMIC_RELAXED);
	// The sender queues its event and then reads asleep; the thread writes
	// asleep and then reads the queue (nap): one of the two sees the other.
	if (__atomic_load_n(&box->asleep, __ATOMIC_SEQ_CST) == 0) {
		return;
	}
	switch (__atomic_exchange_n(&box->asleep, 0, __ATOMIC_SEQ_CST)) {
	case 1:
		futex(&box->asleep, FUTEX_WAKE_PRIVATE, 1, NULL);
		break;
	case 2:
		cairo_sockets_wake(__atomic_load_n(&box->waker, __ATOMIC_SEQ_CST));
		break;
	}
}

cairo_msg *cairo_inbox_drain(int tid)
{
	cairo_mailbox *box = &boxes[tid];
	cairo_msg *list = box->next, *rest = cairo_queue_take(&box->inbox);
	box->next = NULL;
	cairo_msg **end = &list;
	while (*end != NULL) {
		end = &(*end)->next;
	}
	*end = rest;
	int64_t n = 0;
	for (cairo_msg *m = list; m != NULL; m = m->next) {
		n++;
	}
	__atomic_store_n(&box->took, box->took + n, __ATOMIC_RELAXED);
	return list;
}

// The thread's next event, or NULL.
static cairo_msg *take(cairo_mailbox *box)
{
	cairo_msg *m = box->next;
	if (m == NULL && (m = cairo_queue_take(&box->inbox)) == NULL) {
		return NULL;
	}
	box->next = m->next;
	__atomic_store_n(&box->took, box->took + 1, __ATOMIC_RELAXED);   // only this thread writes it
	return m;
}

// The time as Go's time.Now().UnixNano() gives it: dispatch compares the two.
static int64_t unix_ns(void)
{
	struct timespec ts;
	clock_gettime(CLOCK_REALTIME, &ts);
	return (int64_t)ts.tv_sec * 1000000000 + ts.tv_nsec;
}

static int64_t mono_ns(void)
{
	struct timespec ts;
	clock_gettime(CLOCK_MONOTONIC, &ts);
	return (int64_t)ts.tv_sec * 1000000000 + ts.tv_nsec;
}

// look is what a thread does before it sleeps, when it gave Go an operation
// or an answer on its way here: it watches its inbox for look_ns, and not
// past wait, and returns how long it did. Sleeping at once costs two
// wake-ups of threads where this costs none: Go takes over what it was
// given, and the answer of a short operation is there before the thread
// would have slept.
static int64_t look(cairo_mailbox *box, int64_t wait)
{
	int64_t limit = wait < 0 || wait > look_ns ? look_ns : wait;
	int64_t start = mono_ns(), spent = 0;
	while (spent < limit && __atomic_load_n(&box->inbox, __ATOMIC_SEQ_CST) == NULL) {
#if defined(__x86_64__) || defined(__i386__)
		__builtin_ia32_pause();
#elif defined(__aarch64__)
		__asm__ __volatile__("yield");
#endif
		spent = mono_ns() - start;
	}
	return spent;
}

// nap is the thread asleep until an event is queued for it or a socket that
// one of its fibers waits on is ready, for wait nanoseconds at most (below
// 0: as long as it takes). Returns the event, or NULL: then the time is up
// or cairo_sockets_next() has the socket.
static cairo_msg *nap(cairo_mailbox *box, int64_t wait)
{
	__atomic_store_n(&box->since, 0, __ATOMIC_RELAXED);   // asleep in the eyes of dispatch
	int64_t until = wait > 0 ? mono_ns() + wait : 0;
	// With a socket to wait on the thread sleeps in its epoll, where a sender
	// wakes it with a write; with none on a futex, which costs both less.
	int sockets = cairo_sockets_armed() > 0;
	for (;;) {
		__atomic_store_n(&box->asleep, sockets ? 2 : 1, __ATOMIC_SEQ_CST);
		int64_t left = wait > 0 ? until - mono_ns() : 1;
		if (left <= 0 || __atomic_load_n(&box->inbox, __ATOMIC_SEQ_CST) != NULL
			|| (sockets && cairo_sockets_wait(wait > 0 ? left : -1))) {
			__atomic_store_n(&box->asleep, 0, __ATOMIC_SEQ_CST);
			return take(box);
		}
		if (!sockets) {
			struct timespec ts = {.tv_sec = left / 1000000000, .tv_nsec = left % 1000000000};
			// Returns at once when a sender took asleep off in the meantime.
			futex(&box->asleep, FUTEX_WAIT_PRIVATE, 1, wait > 0 ? &ts : NULL);
		}
	}
}

// The thread is back at its loop at now, from a stretch of PHP unless it
// only just started. Returns whether it handed Go something on its way.
static int came_back(cairo_mailbox *box, int tid, int64_t now)
{
	if (box->since != 0) {   // only this thread writes since and stretch, and Go reads them as a hint
		int64_t ran = now - box->since;
		__atomic_store_n(&box->stretch, box->stretch + (ran - box->stretch) / 8, __ATOMIC_RELAXED);
		if (__atomic_load_n(&probed, __ATOMIC_SEQ_CST)) {
			goRan(tid, (long long)ran);
		}
	}
	int handed = box->handed;   // only this thread reads and writes it, here and in its calls into Go
	box->handed = 0;
	return handed;
}

void cairo_loop_answered(int tid)
{
	cairo_mailbox *box = &boxes[tid];
	box->handed = 1;
	if (box->since != 0 && __atomic_load_n(&probed, __ATOMIC_SEQ_CST)) {
		goRan(tid, (long long)(unix_ns() - box->since));
	}
}

// Whether the kernel has a socket ready for the thread, asked without
// waiting. An idle thread with nothing queued does not ask: it is about to
// sleep in its epoll, which answers the same.
static int sockets_ready(cairo_mailbox *box, zend_long wait)
{
	return cairo_sockets_armed() > 0
		&& (wait == 0 || box->next != NULL || __atomic_load_n(&box->inbox, __ATOMIC_SEQ_CST) != NULL)
		&& cairo_sockets_wait(0);
}

// The thread's next event from Go: what is queued, or what comes within
// wait nanoseconds. NULL when a socket came first or the time is up. *now
// follows the clock.
static cairo_msg *next_msg(cairo_mailbox *box, zend_long wait, int handed, int64_t *now)
{
	cairo_msg *m = take(box);
	if (m == NULL && wait != 0 && handed) {
		int64_t spent = look(box, wait);
		*now += spent;
		if (wait > 0) {
			wait = wait > spent ? wait - spent : 0;
		}
		m = take(box);
	}
	if (m == NULL && wait != 0) {
		m = nap(box, wait);
		*now = unix_ns();
	}
	return m;
}

// cairo_next_event(int $wait, bool $sockets = true): array|int|null
// The thread comes back to its loop. Returns a socket that is ready for the
// fiber that waits on it (cairo_socket_arm() in sockets.c), as its
// descriptor; or the next event Go has for the thread (a request, a
// finished operation, shutdown), which Go encodes in internal/wire's format
// and wire.c turns into the array, or what another PHP thread has for it (an
// offload job or its result, offload.h); or null when neither came within $wait
// nanoseconds (0: do not wait; below 0: wait as long as it takes). Without
// $sockets it does not ask the kernel for ready sockets, and still returns
// those it asked for before: one question a turn of the loop is enough.
PHP_FUNCTION(cairo_next_event)
{
	zend_long wait;
	bool sockets = 1;
	ZEND_PARSE_PARAMETERS_START(1, 2)
		Z_PARAM_LONG(wait)
		Z_PARAM_OPTIONAL
		Z_PARAM_BOOL(sockets)
	ZEND_PARSE_PARAMETERS_END();
	int tid = cairo_thread_id();
	if (tid < 0) {
		zend_throw_error(NULL, "cairo_next_event(): this thread has no events");
		RETURN_THROWS();
	}
	cairo_mailbox *box = &boxes[tid];
	int64_t now = unix_ns();
	int handed = came_back(box, tid, now);
	int fd = cairo_sockets_next();
	if (fd < 0 && sockets && sockets_ready(box, wait)) {
		fd = cairo_sockets_next();
	}
	cairo_msg *m = fd < 0 ? next_msg(box, wait, handed, &now) : NULL;
	if (fd < 0 && m == NULL) {
		fd = cairo_sockets_next();   // what ended the sleep
	}
	__atomic_store_n(&box->since, now, __ATOMIC_RELAXED);
	if (fd >= 0) {
		RETURN_LONG(fd);
	}
	if (m == NULL) {
		RETURN_NULL();
	}
	if (m->kind == CAIRO_EVENT_CALL || m->kind == CAIRO_EVENT_JOB) {
		__atomic_store_n(&box->served, box->served + 1, __ATOMIC_RELAXED);
	}
	if (__atomic_load_n(&probed, __ATOMIC_SEQ_CST)) {
		goTook(tid, (long long)m->id);
	}
	int ok = m->kind == CAIRO_EVENT_JOB || m->kind == CAIRO_EVENT_DONE
		? cairo_offload_event(tid, m, return_value)
		: cairo_wire_decode(m->data, m->len, return_value);
	free(m);
	if (ok == FAILURE) {
		zend_throw_exception(NULL, "cairo: malformed event", 0);
		RETURN_THROWS();
	}
}

// cairo_wait_began(int $request): int
// A fiber of the request starts a wait that stays on the thread (Waits in waits.php), which
// Go does not see. Returns what cairo_wait_ended() takes back; 0 when
// requests are not probed, and then neither function calls Go.
PHP_FUNCTION(cairo_wait_began)
{
	zend_long request;
	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_LONG(request)
	ZEND_PARSE_PARAMETERS_END();
	if (!__atomic_load_n(&probed, __ATOMIC_SEQ_CST)) {
		RETURN_LONG(0);
	}
	RETURN_LONG((zend_long)goWaitBegan(cairo_thread_id(), (long long)request));
}

// cairo_wait_ended(int $request, int $kind, int $began): void
// The wait that cairo_wait_began() answered $began for is over; $kind is
// what the fiber waited in (the constants of Waits).
PHP_FUNCTION(cairo_wait_ended)
{
	zend_long request, kind, began;
	ZEND_PARSE_PARAMETERS_START(3, 3)
		Z_PARAM_LONG(request)
		Z_PARAM_LONG(kind)
		Z_PARAM_LONG(began)
	ZEND_PARSE_PARAMETERS_END();
	goWaitEnded(cairo_thread_id(), (long long)request, (int)kind, (long long)began);
}
