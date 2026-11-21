// The loop of a PHP thread, where it comes back between fibers: it takes the
// events Go queued for it and, with none and no fiber to run, sleeps, all
// without a call into Go. See loop.h.

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

int cairo_loop_has_event(void)
{
	int tid = cairo_thread_id();
	return tid >= 0 && (boxes[tid].next != NULL || __atomic_load_n(&boxes[tid].inbox, __ATOMIC_SEQ_CST) != NULL);
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
	m->tid = tid;
	m->kind = kind;
	m->len = (uint32_t)n;
	m->blen = 0;
	memcpy(m->data, data, n);
	cairo_mailbox *box = &boxes[tid];
	cairo_queue_push(&box->inbox, m);
	__atomic_fetch_add(&box->pushed, 1, __ATOMIC_RELAXED);
	// The sender queues its event and then reads asleep; the thread writes
	// asleep and then reads the queue (nap): one of the two sees the other.
	if (__atomic_load_n(&box->asleep, __ATOMIC_SEQ_CST) != 0 && __atomic_exchange_n(&box->asleep, 0, __ATOMIC_SEQ_CST) != 0) {
		futex(&box->asleep, FUTEX_WAKE_PRIVATE, 1, NULL);
	}
	return 0;
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

// nap is the thread asleep until an event is queued for it, for wait
// nanoseconds at most (below 0: as long as it takes). Returns the event, or
// NULL.
static cairo_msg *nap(cairo_mailbox *box, int64_t wait)
{
	__atomic_store_n(&box->since, 0, __ATOMIC_RELAXED);   // asleep in the eyes of dispatch
	int64_t until = wait > 0 ? mono_ns() + wait : 0;
	for (;;) {
		__atomic_store_n(&box->asleep, 1, __ATOMIC_SEQ_CST);
		int64_t left = wait > 0 ? until - mono_ns() : 1;
		if (left <= 0 || __atomic_load_n(&box->inbox, __ATOMIC_SEQ_CST) != NULL) {
			__atomic_store_n(&box->asleep, 0, __ATOMIC_SEQ_CST);
			return take(box);
		}
		struct timespec ts = {.tv_sec = left / 1000000000, .tv_nsec = left % 1000000000};
		// Returns at once when a sender took asleep off in the meantime.
		futex(&box->asleep, FUTEX_WAIT_PRIVATE, 1, wait > 0 ? &ts : NULL);
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

// cairo_next_event(int $wait): ?array
// The thread comes back to its loop. Returns the next event Go has for it
// (a request, a finished operation, shutdown), which Go encodes in
// internal/wire's format and wire.c turns into the array; or null when
// none came within $wait nanoseconds (0: do not wait; below 0: wait as long
// as it takes).
PHP_FUNCTION(cairo_next_event)
{
	zend_long wait;
	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_LONG(wait)
	ZEND_PARSE_PARAMETERS_END();
	int tid = cairo_thread_id();
	if (tid < 0) {
		zend_throw_error(NULL, "cairo_next_event(): this thread has no events");
		RETURN_THROWS();
	}
	cairo_mailbox *box = &boxes[tid];
	int64_t now = unix_ns();
	int handed = came_back(box, tid, now);
	cairo_msg *m = take(box);
	if (m == NULL && wait != 0 && handed) {
		int64_t spent = look(box, wait);
		now += spent;
		if (wait > 0) {
			wait = wait > spent ? wait - spent : 0;
		}
		m = take(box);
	}
	if (m == NULL && wait != 0) {
		m = nap(box, wait);
		now = unix_ns();
	}
	__atomic_store_n(&box->since, now, __ATOMIC_RELAXED);
	if (m == NULL) {
		RETURN_NULL();
	}
	if (m->kind == CAIRO_EVENT_CALL) {
		__atomic_store_n(&box->served, box->served + 1, __ATOMIC_RELAXED);
	}
	if (__atomic_load_n(&probed, __ATOMIC_SEQ_CST)) {
		goTook(tid, (long long)m->id);
	}
	int ok = cairo_wire_decode(m->data, m->len, return_value);
	free(m);
	if (ok == FAILURE) {
		zend_throw_exception(NULL, "cairo: malformed event from Go", 0);
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

// cairo_wait_ended(int $request, bool $parked, int $began): void
// The wait that cairo_wait_began() answered $began for is over; $parked
// tells a wait in select() from a sleep.
PHP_FUNCTION(cairo_wait_ended)
{
	zend_long request, began;
	bool parked;
	ZEND_PARSE_PARAMETERS_START(3, 3)
		Z_PARAM_LONG(request)
		Z_PARAM_BOOL(parked)
		Z_PARAM_LONG(began)
	ZEND_PARSE_PARAMETERS_END();
	goWaitEnded(cairo_thread_id(), (long long)request, parked ? 1 : 0, (long long)began);
}
