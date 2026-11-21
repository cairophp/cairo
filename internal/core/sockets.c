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

#include "extension.h"
#include "loop.h"
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

void cairo_sockets_thread_end(void)
{
	if (epfd >= 0) {
		close(epfd);
	}
	epfd = waker = -1;
	armed = nready = taken = 0;
}

int cairo_sockets_next(void)
{
	while (taken < nready) {
		int fd = ready[taken++].data.fd;
		if (fd >= 0 && fd != waker) {
			armed--;
			return fd;
		}
	}
	return -1;
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

// cairo_socket_arm(int $fd, int $events): ?string
// A fiber of this thread waits for socket $fd to be readable (1), writable
// (2) or either (3): the loop gets $fd from cairo_next_event() once it is.
// Returns null, or why the socket cannot be waited on. The socket stays in
// the epoll afterwards, switched off (EPOLLONESHOT), so the next wait on it
// is one system call; the kernel drops it when PHP closes it.
PHP_FUNCTION(cairo_socket_arm)
{
	zend_long fd, events;
	ZEND_PARSE_PARAMETERS_START(2, 2)
		Z_PARAM_LONG(fd)
		Z_PARAM_LONG(events)
	ZEND_PARSE_PARAMETERS_END();
	if (fd < 0 || fd > INT_MAX || (events & 3) == 0) {
		RETURN_STRING("not a socket to wait on");
	}
	if (epfd < 0 && open_epoll() != 0) {
		RETURN_STRING(strerror(errno));
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
		RETURN_STRING(strerror(errno));
	}
	armed++;
}

// cairo_socket_disarm(int $fd): void
// The wait on socket $fd, which cairo_socket_arm() began and
// cairo_next_event() has not returned, is over: its fiber no longer waits.
PHP_FUNCTION(cairo_socket_disarm)
{
	zend_long fd;
	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_LONG(fd)
	ZEND_PARSE_PARAMETERS_END();
	if (epfd < 0 || fd < 0 || fd > INT_MAX) {
		return;
	}
	// Out of the epoll, not only switched off: an error on the socket would
	// still be reported, to whoever waits on that number next.
	epoll_ctl(epfd, EPOLL_CTL_DEL, (int)fd, NULL);
	for (int i = taken; i < nready; i++) {
		if (ready[i].data.fd == (int)fd) {
			ready[i].data.fd = -1;   // found ready, and not taken yet
		}
	}
	armed--;
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
