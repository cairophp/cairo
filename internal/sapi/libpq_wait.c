// libpq's waits on a fiber.
//
// PHP's pdo_pgsql talks to PostgreSQL through libpq, which waits for its
// socket in poll() whenever a query, a connect, a COPY or a large object
// needs the server's answer. Here that poll() parks the calling fiber on the
// thread's epoll (cairo_hook_wait(), as the stream hooks do for mysqlnd), so
// the thread runs its other fibers meanwhile. Every other caller of poll()
// is left alone: a library that keeps per-thread state across its wait (the
// resolver of getaddrinfo()) would break if another fiber of the thread ran
// in the middle of it.
//
// The binary defines poll() itself. Where libpq is a shared library the
// dynamic linker binds libpq's calls to it (the executable comes first),
// and it takes the calls whose return address lies in libpq's code. The
// static binary links libpq in, and scripts/build-static.sh renames libpq's
// calls to poll() into calls to cairo_libpq_poll() (CAIRO_STATIC_LIBPQ).

#ifndef _GNU_SOURCE
#define _GNU_SOURCE
#endif
#include <errno.h>
#include <poll.h>
#include <pthread.h>
#include <stdint.h>
#include <string.h>
#include <sys/socket.h>

#include <php.h>

#include "hooks.h"
#include "libpq_wait.h"

#if defined(CAIRO_STATIC_LIBPQ) && !defined(__GLIBC__)
#include <signal.h>
#include <sys/syscall.h>
#include <unistd.h>

// The static musl binary has no other poll() than this file's.
static int real_poll(struct pollfd *fds, nfds_t nfds, int timeout)
{
	struct timespec ts, *at = NULL;
	if (timeout >= 0) {
		ts.tv_sec = timeout / 1000;
		ts.tv_nsec = (long)(timeout % 1000) * 1000000;
		at = &ts;
	}
	return (int)syscall(SYS_ppoll, fds, nfds, at, NULL, (size_t)(_NSIG / 8));
}

static void find_real_poll(void)
{
}
#else
#include <dlfcn.h>

static int (*real_poll)(struct pollfd *, nfds_t, int);

static void find_real_poll(void)
{
	real_poll = (int (*)(struct pollfd *, nfds_t, int))dlsym(RTLD_NEXT, "poll");
}
#endif

#ifdef CAIRO_STATIC_LIBPQ
// libpq's calls are renamed to cairo_libpq_poll() at the build.
static int from_libpq(uintptr_t ra)
{
	(void)ra;
	return 0;
}

void cairo_libpq_settle(void)
{
}
#else
#include <link.h>

static uintptr_t libpq_lo, libpq_hi;   // libpq's executable segment, once loaded

static int find_libpq(struct dl_phdr_info *info, size_t size, void *data)
{
	(void)size;
	(void)data;
	if (info->dlpi_name == NULL || strstr(info->dlpi_name, "/libpq.so") == NULL) {
		return 0;
	}
	for (int i = 0; i < info->dlpi_phnum; i++) {
		const ElfW(Phdr) *ph = &info->dlpi_phdr[i];
		if (ph->p_type == PT_LOAD && (ph->p_flags & PF_X)) {
			uintptr_t lo = info->dlpi_addr + ph->p_vaddr;
			__atomic_store_n(&libpq_hi, lo + ph->p_memsz, __ATOMIC_RELAXED);
			__atomic_store_n(&libpq_lo, lo, __ATOMIC_RELEASE);
			return 1;
		}
	}
	return 0;
}

static int libpq_settled;   // looked for after PHP's startup: no later look

// Whether the code at return address ra is libpq's. pdo_pgsql.so may load
// libpq after the process started (the native image's php.ini), so until
// PHP's startup has ended a PHP thread that finds no libpq yet looks again.
static int from_libpq(uintptr_t ra)
{
	uintptr_t lo = __atomic_load_n(&libpq_lo, __ATOMIC_ACQUIRE);
	if (lo == 0) {
		if (__atomic_load_n(&libpq_settled, __ATOMIC_ACQUIRE) || tsrm_get_ls_cache() == NULL) {
			return 0;
		}
		dl_iterate_phdr(find_libpq, NULL);
		lo = __atomic_load_n(&libpq_lo, __ATOMIC_ACQUIRE);
	}
	return lo != 0 && ra >= lo && ra < __atomic_load_n(&libpq_hi, __ATOMIC_RELAXED);
}

// After PHP's startup every extension of php.ini is loaded, and a server
// has no dl(): libpq is there now or never is. Without this, an app with no
// pdo_pgsql walked the list of loaded libraries on each poll() of a PHP
// thread (mysqlnd's waits among them).
void cairo_libpq_settle(void)
{
	dl_iterate_phdr(find_libpq, NULL);
	__atomic_store_n(&libpq_settled, 1, __ATOMIC_RELEASE);
}
#endif

static pthread_once_t found_poll = PTHREAD_ONCE_INIT;

// Every poll() of the process comes here: libpq's calls, and pdo_pgsql's
// wait in getNotify() (pdo_pgsql_defaults.c), go to cairo_libpq_poll().
int poll(struct pollfd *fds, nfds_t nfds, int timeout)
{
	pthread_once(&found_poll, find_real_poll);
	if (from_libpq((uintptr_t)__builtin_return_address(0))
		|| (tsrm_get_ls_cache() != NULL && cairo_pgsql_notify_wait())) {
		return cairo_libpq_poll(fds, nfds, timeout);
	}
	return real_poll(fds, nfds, timeout);
}

int cairo_libpq_poll(struct pollfd *fds, nfds_t nfds, int timeout)
{
	// libpq waits on one socket, for reading or writing (both while it
	// sends: writable is what it waits for then, and it reads when it can).
	pthread_once(&found_poll, find_real_poll);
	if (nfds != 1 || timeout == 0 || tsrm_get_ls_cache() == NULL || cairo_thread_id() < 0) {
		return real_poll(fds, nfds, timeout);
	}
	int write = (fds->events & POLLOUT) != 0;
	switch (cairo_hook_wait(fds->fd, write, timeout < 0 ? -1.0 : timeout / 1000.0, true)) {
		case 1:
			fds->revents = (short)(fds->events & (POLLIN | POLLOUT));
			return 1;
		case -1:
			fds->revents = 0;
			return 0;
		case -2:
			// The request's context ended (cancelled, past its deadline):
			// libpq fails the call, and the exception is the one PHP sees.
			// The server still answers the query, and libpq, which thinks
			// it done, would read that answer as the next query's: so the
			// connection is shut, and its next query fails instead.
			shutdown(fds->fd, SHUT_RDWR);
			errno = ECANCELED;
			return -1;
		default:
			return real_poll(fds, nfds, timeout);
	}
}
