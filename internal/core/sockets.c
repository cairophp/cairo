// The sockets that fibers of a PHP thread wait on, in the thread's own
// epoll. See sockets.h.

#include <errno.h>
#include <poll.h>
#include <string.h>
#include <sys/epoll.h>
#include <sys/eventfd.h>
#include <sys/syscall.h>
#include <time.h>
#include <unistd.h>

#include <php.h>
#include <ext/spl/spl_exceptions.h>
#include <zend_exceptions.h>

#include "extension.h"
#include "loop.h"
#include "parked.h"
#include "sockets.h"

// epoll_pwait2 (Linux 5.11) takes its timeout in nanoseconds, which the
// thread's timers need; it has one number on every architecture.
#ifndef SYS_epoll_pwait2
#define SYS_epoll_pwait2 441
#endif

// How many ready sockets one question to the kernel takes.
#define CAIRO_SOCKETS_BATCH 64

static __thread int epfd = -1;   // the thread's epoll, once a fiber waited on a socket
static __thread int waker = -1;  // the descriptor in it that wakes the thread
static __thread int armed;       // sockets armed and not yet taken as ready or disarmed
static __thread HashTable watched;   // with an epoll: armed socket => the wait of the fiber that waits on it (parked.h)
static __thread struct epoll_event ready[CAIRO_SOCKETS_BATCH];
static __thread int nready, taken;
static int no_pwait2;            // the kernel has no epoll_pwait2: timeouts are rounded up to milliseconds

int cairo_sockets_armed(void)
{
	return armed;
}

// Opens the calling thread's epoll. What wakes the thread from it is an
// eventfd that belongs to the thread's id and is never closed: a sender that
// read it from the mailbox may write it after the thread has ended.
static int open_epoll(void)
{
	int tid = cairo_thread_id();
	if (tid < 0) {
		errno = EPERM;
		return -1;
	}
	cairo_mailbox *box = cairo_mailbox_of(tid);
	int fd = __atomic_load_n(&box->waker, __ATOMIC_SEQ_CST) - 1;
	if (fd < 0) {
		fd = eventfd(0, EFD_NONBLOCK | EFD_CLOEXEC);
		if (fd < 0) {
			return -1;
		}
		__atomic_store_n(&box->waker, fd + 1, __ATOMIC_SEQ_CST);
	}
	int ep = epoll_create1(EPOLL_CLOEXEC);
	if (ep < 0) {
		return -1;
	}
	// Edge-triggered and never read: each write is one wake-up.
	struct epoll_event ev = {.events = EPOLLIN | EPOLLET, .data.fd = fd};
	if (epoll_ctl(ep, EPOLL_CTL_ADD, fd, &ev) != 0) {
		close(ep);
		return -1;
	}
	zend_hash_init(&watched, 64, NULL, NULL, 0);
	epfd = ep;
	waker = fd;
	return 0;
}

void cairo_sockets_wake(int32_t w)
{
	uint64_t one = 1;
	if (w > 0 && write(w - 1, &one, sizeof(one)) < 0) {
		// Full, after 2^64 writes nobody read: the thread is awake by then.
	}
}

void cairo_sockets_request_end(void)
{
	if (epfd >= 0) {
		close(epfd);
	}
	epfd = waker = -1;
	armed = nready = taken = 0;
}

int cairo_sockets_take(bool all)
{
	int woke = 0;
	while (taken < nready && (all || woke == 0)) {
		int fd = ready[taken++].data.fd;
		if (fd < 0 || fd == waker) {
			continue;
		}
		armed--;
		zval *wait = zend_hash_index_find(&watched, (zend_ulong)fd);
		if (wait != NULL) {
			zend_long id = Z_LVAL_P(wait);
			zend_hash_index_del(&watched, (zend_ulong)fd);
			cairo_parked_wake(id);
			woke++;
		}
	}
	return woke;
}

static int wait_ns(int64_t ns)
{
	if (!no_pwait2) {
		struct timespec ts = {.tv_sec = ns / 1000000000, .tv_nsec = ns % 1000000000};
		int n = (int)syscall(SYS_epoll_pwait2, epfd, ready, CAIRO_SOCKETS_BATCH, ns < 0 ? NULL : &ts, NULL, (size_t)0);
		// A sandbox that does not know the call refuses it with EPERM.
		if (n >= 0 || (errno != ENOSYS && errno != EPERM)) {
			return n;
		}
		no_pwait2 = 1;
	}
	return epoll_wait(epfd, ready, CAIRO_SOCKETS_BATCH, ns < 0 ? -1 : (int)((ns + 999999) / 1000000));
}

int cairo_sockets_wait(int64_t ns)
{
	if (taken < nready || epfd < 0) {
		return taken < nready;
	}
	int n = wait_ns(ns);   // an interrupted wait is a wake-up for nothing
	nready = n > 0 ? n : 0;
	taken = 0;
	for (int i = 0; i < nready; i++) {
		if (ready[i].data.fd != waker) {
			return 1;
		}
	}
	nready = 0;
	return 0;
}

// The fiber of wait id waits for socket fd to be readable (events 1),
// writable (2) or either (3). Returns NULL, or why the socket cannot be
// waited on. The socket stays in the epoll afterwards, switched off
// (EPOLLONESHOT), so the next wait on it is one system call; the kernel
// drops it when PHP closes it.
static const char *arm(zend_long fd, zend_long events, zend_long id)
{
	if (fd < 0 || fd > INT_MAX || (events & 3) == 0) {
		return "not a socket to wait on";
	}
	if (epfd < 0 && open_epoll() != 0) {
		return strerror(errno);
	}
	if (zend_hash_index_exists(&watched, (zend_ulong)fd)) {
		return "socket is already in use by another fiber";
	}
	struct epoll_event ev = {.events = EPOLLONESHOT, .data.fd = (int)fd};
	if (events & 1) {
		ev.events |= EPOLLIN | EPOLLRDHUP;
	}
	if (events & 2) {
		ev.events |= EPOLLOUT;
	}
	if (epoll_ctl(epfd, EPOLL_CTL_MOD, (int)fd, &ev) != 0
		&& (errno != ENOENT || epoll_ctl(epfd, EPOLL_CTL_ADD, (int)fd, &ev) != 0)) {
		return strerror(errno);
	}
	zval wait;
	ZVAL_LONG(&wait, id);
	zend_hash_index_add_new(&watched, (zend_ulong)fd, &wait);
	armed++;
	return NULL;
}

// Wait id is over. Returns whether socket fd was still armed for it, which
// it no longer is; false when the loop found the socket ready, or the wait
// never armed it.
static bool disarm(zend_long fd, zend_long id)
{
	zval *wait = epfd >= 0 ? zend_hash_index_find(&watched, (zend_ulong)fd) : NULL;
	if (wait == NULL || Z_LVAL_P(wait) != id) {
		return false;
	}
	zend_hash_index_del(&watched, (zend_ulong)fd);
	// Out of the epoll, not only switched off: an error on the socket would
	// still be reported, to whoever waits on that number next.
	epoll_ctl(epfd, EPOLL_CTL_DEL, (int)fd, NULL);
	for (int i = taken; i < nready; i++) {
		if (ready[i].data.fd == (int)fd) {
			ready[i].data.fd = -1;   // found ready, and not taken yet
		}
	}
	armed--;
	return true;
}

// cairo_wait_socket(int $fd, int $events, int $id, object $context, int $request, int $at): bool
// Parks the running fiber as wait $id (cairo_park() in parked.c) until
// socket $fd is readable ($events 1), writable (2) or either (3), and says
// whether it is: false when the wait ended first, at $at or by
// cairo_wake($id). Throws when the socket cannot be waited on, which
// includes that another fiber waits on it.
PHP_FUNCTION(cairo_wait_socket)
{
	zend_long fd, events, id, request, at;
	zend_object *context;
	ZEND_PARSE_PARAMETERS_START(6, 6)
		Z_PARAM_LONG(fd)
		Z_PARAM_LONG(events)
		Z_PARAM_LONG(id)
		Z_PARAM_OBJ(context)
		Z_PARAM_LONG(request)
		Z_PARAM_LONG(at)
	ZEND_PARSE_PARAMETERS_END();
	const char *error = arm(fd, events, id);
	if (error != NULL) {
		zend_throw_exception(spl_ce_RuntimeException, error, 0);
		RETURN_THROWS();
	}
	cairo_park_fiber(id, context, request, at, CAIRO_WAIT_SOCKET);
	RETURN_BOOL(!disarm(fd, id));
}

// cairo_wait_sockets(array $events, int $id, object $context, int $request, int $at): bool
// cairo_wait_socket() for the first of several sockets: $events is socket
// => 1, 2 or 3. With none the fiber waits out the time.
PHP_FUNCTION(cairo_wait_sockets)
{
	HashTable *sockets;
	zend_long id, request, at;
	zend_object *context;
	ZEND_PARSE_PARAMETERS_START(5, 5)
		Z_PARAM_ARRAY_HT(sockets)
		Z_PARAM_LONG(id)
		Z_PARAM_OBJ(context)
		Z_PARAM_LONG(request)
		Z_PARAM_LONG(at)
	ZEND_PARSE_PARAMETERS_END();
	const char *error = NULL;
	zend_ulong fd;
	zval *events;
	ZEND_HASH_FOREACH_NUM_KEY_VAL(sockets, fd, events) {
		error = Z_TYPE_P(events) == IS_LONG ? arm((zend_long)fd, Z_LVAL_P(events), id) : "not a socket to wait on";
		if (error != NULL) {
			break;
		}
	} ZEND_HASH_FOREACH_END();
	if (error == NULL) {
		cairo_park_fiber(id, context, request, at, CAIRO_WAIT_SOCKETS);
	}
	bool ready = false;
	ZEND_HASH_FOREACH_NUM_KEY(sockets, fd) {
		ready = !disarm((zend_long)fd, id) || ready;
	} ZEND_HASH_FOREACH_END();
	if (error != NULL) {
		zend_throw_exception(spl_ce_RuntimeException, error, 0);
		RETURN_THROWS();
	}
	RETURN_BOOL(ready);
}

// Longest wait on the PHP thread itself, in milliseconds: long enough for a
// database on the same network to answer, short enough that a cancelled
// request is not held up noticeably.
#define CAIRO_WAIT_HERE_MS 1

// cairo_wait_here(int $fd, bool $write): bool
// Waits for the socket with the thread stopped, and says whether it became
// ready: one poll() where a wait through the loop is two system calls and
// two switches of fibers. The scheduler calls it only where the thread
// could run nothing else meanwhile (Scheduler::waitHere).
PHP_FUNCTION(cairo_wait_here)
{
	zend_long fd;
	bool write;
	ZEND_PARSE_PARAMETERS_START(2, 2)
		Z_PARAM_LONG(fd)
		Z_PARAM_BOOL(write)
	ZEND_PARSE_PARAMETERS_END();
	if (fd < 0 || fd > INT_MAX) {
		RETURN_FALSE;
	}
	struct pollfd pfd = {.fd = (int)fd, .events = write ? POLLOUT : POLLIN};
	RETURN_BOOL(poll(&pfd, 1, CAIRO_WAIT_HERE_MS) > 0);
}
