// The loop of a PHP thread, where it comes back between fibers: it wakes the
// fibers whose sockets are ready, takes the events Go queued for it and,
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
#include "fibers.h"
#include "offload.h"
#include "parked.h"
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

// A message for an event of n bytes, which the caller fills, or NULL.
static cairo_msg *new_event(int kind, int64_t id, size_t n)
{
	cairo_msg *m = n > INT32_MAX ? NULL : malloc(sizeof(*m) + n);
	if (m != NULL) {
		m->id = id;
		m->kind = kind;
		m->len = (uint32_t)n;
		m->blen = 0;
	}
	return m;
}

int cairo_inbox_push(int tid, int kind, int64_t id, const char *data, size_t n)
{
	cairo_msg *m = new_event(kind, id, n);
	if (m == NULL) {
		return -1;
	}
	memcpy(m->data, data, n);
	cairo_inbox_queue(tid, m);
	return 0;
}

int cairo_inbox_call(int tid, int64_t id, const char *data, size_t n, const char *tail, size_t tn)
{
	cairo_msg *m = tn > INT32_MAX ? NULL : new_event(CAIRO_EVENT_CALL, id, n + tn);
	if (m == NULL) {
		return -1;
	}
	memcpy(m->data, data, n);
	if (tn > 0) {
		memcpy(m->data + n, tail, tn);
	}
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

// Classic mode: the PHP request has its request, and the calls that come
// after it are the next PHP request's (cairo_hold_calls()).
static __thread bool holding;

// The thread's next event that is not a call it holds, or NULL. The calls
// it passes stay in next, in their order.
static cairo_msg *take_held(cairo_mailbox *box)
{
	cairo_msg **end = &box->next;
	while (*end != NULL) {
		end = &(*end)->next;
	}
	*end = cairo_queue_take(&box->inbox);
	for (cairo_msg **at = &box->next; *at != NULL; at = &(*at)->next) {
		if ((*at)->kind != CAIRO_EVENT_CALL) {
			cairo_msg *m = *at;
			*at = m->next;
			__atomic_store_n(&box->took, box->took + 1, __ATOMIC_RELAXED);
			return m;
		}
	}
	return NULL;
}

// cairo_hold_calls(): void
// The calls that come from now on stay in the inbox for the PHP request that
// follows this one on the thread (classic.h).
PHP_FUNCTION(cairo_hold_calls)
{
	ZEND_PARSE_PARAMETERS_NONE();
	holding = true;
}

// The thread's next event, or NULL.
static cairo_msg *take(cairo_mailbox *box)
{
	if (holding) {
		return take_held(box);
	}
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
// or cairo_sockets_take() has the socket.
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

// Whether the thread has run no fiber since it left its loop: the first one
// it resumes began with the stretch, and its time is the mailbox's since.
static __thread bool fresh;

void cairo_loop_request_end(void)
{
	fresh = holding = false;
}

void cairo_loop_resumes(void)
{
	if (fresh) {
		fresh = 0;
		return;
	}
	int tid = cairo_thread_id();
	if (tid >= 0) {
		__atomic_store_n(&boxes[tid].fiber, unix_ns(), __ATOMIC_RELAXED);
	}
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
static int sockets_ready(cairo_mailbox *box, int64_t wait)
{
	return cairo_sockets_armed() > 0
		&& (wait == 0 || box->next != NULL || __atomic_load_n(&box->inbox, __ATOMIC_SEQ_CST) != NULL)
		&& cairo_sockets_wait(0);
}

// The thread's next event from Go: what is queued, or what comes within
// wait nanoseconds. NULL when a socket came first or the time is up. *now
// follows the clock.
static cairo_msg *next_msg(cairo_mailbox *box, int64_t wait, int handed, int64_t *now)
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

// The thread has run server.php and come to its loop: Go may give it the
// requests of the thread it replaces (retire in supervise.go).
static void at_loop(int tid)
{
	static __thread bool ready;
	if (!ready) {
		ready = 1;
		cairo_outbox_ready(tid);
	}
}

// The thread took m out of its loop: dispatch and the probes learn of it.
static void took(cairo_mailbox *box, int tid, cairo_msg *m, int64_t now)
{
	__atomic_store_n(&box->since, now, __ATOMIC_RELAXED);
	if (m->kind == CAIRO_EVENT_CALL || m->kind == CAIRO_EVENT_JOB) {
		__atomic_store_n(&box->served, box->served + 1, __ATOMIC_RELAXED);
	}
	if (__atomic_load_n(&probed, __ATOMIC_SEQ_CST)) {
		goTook(tid, (long long)m->id);
	}
}

cairo_msg *cairo_loop_take_call(int tid)
{
	at_loop(tid);
	cairo_mailbox *box = &boxes[tid];
	int64_t now = unix_ns();
	int handed = came_back(box, tid, now);
	cairo_msg *m;
	while ((m = next_msg(box, -1, handed, &now)) == NULL) {
		handed = 0;
	}
	if (m->kind != CAIRO_EVENT_CALL) {
		// Back where take() finds it first, as if it had not been taken.
		m->next = box->next;
		box->next = m;
		__atomic_store_n(&box->took, box->took - 1, __ATOMIC_RELAXED);
		__atomic_store_n(&box->since, now, __ATOMIC_RELAXED);
		return NULL;
	}
	took(box, tid, m, now);
	return m;
}

// cairo_next_event(bool $idle, bool $turn = true): ?array
// The thread comes back to its loop. Returns the next event Go has for the
// thread (a request, a finished operation, shutdown), which Go encodes in
// internal/wire's format and wire.c turns into the array, or what another
// PHP thread has for it (an offload job or its result, offload.h); or null.
// A thread that is $idle (PHP has nothing to run) waits for an event when no
// fiber is ready either, until a socket that a fiber waits on is ready
// (sockets.h) or the next timer of a parked fiber at the latest. Each turn
// of the loop begins with a call that is its $turn: that one wakes the
// parked fibers whose time is up (parked.h) and asks the kernel which
// sockets are ready, whose fibers it wakes: all of them when PHP has more to
// run, else one a call, and then it returns no event with it. The turn's
// other calls only return what is there.
PHP_FUNCTION(cairo_next_event)
{
	bool idle, turn = 1;
	ZEND_PARSE_PARAMETERS_START(1, 2)
		Z_PARAM_BOOL(idle)
		Z_PARAM_OPTIONAL
		Z_PARAM_BOOL(turn)
	ZEND_PARSE_PARAMETERS_END();
	int tid = cairo_thread_id();
	if (tid < 0) {
		zend_throw_error(NULL, "cairo_next_event(): this thread has no events");
		RETURN_THROWS();
	}
	at_loop(tid);
	cairo_mailbox *box = &boxes[tid];
	int64_t now = unix_ns();
	int handed = came_back(box, tid, now);
	int64_t wait = turn ? cairo_parked_tick() : 0;
	if (!idle || cairo_fibers_queued() > 0) {
		wait = 0;
	}
	// A thread with nothing else to run takes one socket a turn and no event
	// with it. The fiber that waited goes on before a new request begins:
	// with requests begun ahead of it, Laravel's dashboard ran 5 to 8%
	// slower. And one a turn keeps a stretch of PHP one fiber's, which is
	// what the blocking detector reads it as.
	int woke = cairo_sockets_take(!idle) || (turn && sockets_ready(box, wait) && cairo_sockets_take(!idle));
	cairo_msg *m = woke && idle ? NULL : next_msg(box, woke ? 0 : wait, handed, &now);
	if (m == NULL && !woke) {
		cairo_sockets_take(!idle);   // what ended the thread's sleep
	}
	fresh = m == NULL;   // PHP runs the fiber of an event before those that are ready
	if (m == NULL) {
		__atomic_store_n(&box->since, now, __ATOMIC_RELAXED);
		RETURN_NULL();
	}
	took(box, tid, m, now);
	int ok = m->kind == CAIRO_EVENT_JOB || m->kind == CAIRO_EVENT_DONE
		? cairo_offload_event(tid, m, return_value)
		: cairo_wire_decode(m->data, m->len, return_value);
	free(m);
	if (ok == FAILURE) {
		zend_throw_exception(NULL, "cairo: malformed event", 0);
		RETURN_THROWS();
	}
}

// cairo_stuck(): bool
// Whether nothing on the calling thread can end a wait any more: no fiber
// is ready, no parked fiber has a time that comes and none waits on a
// socket. What Go still has to send does not count: the caller knows of that.
PHP_FUNCTION(cairo_stuck)
{
	ZEND_PARSE_PARAMETERS_NONE();
	RETURN_BOOL(cairo_parked_tick() < 0 && cairo_fibers_queued() == 0 && cairo_sockets_armed() == 0);
}

int64_t cairo_wait_began(int64_t request)
{
	if (!__atomic_load_n(&probed, __ATOMIC_SEQ_CST)) {
		return 0;
	}
	return (int64_t)goWaitBegan(cairo_thread_id(), (long long)request);
}

void cairo_wait_ended(int64_t request, int kind, int64_t began)
{
	goWaitEnded(cairo_thread_id(), (long long)request, kind, (long long)began);
}
