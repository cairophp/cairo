// Cairo's curl hook.
//
// PHP's curl_exec() ends in curl_easy_perform(), which blocks the thread while
// libcurl polls its sockets. Cairo defines curl_easy_perform in the binary, so
// libphp's call resolves here first (the executable is searched before
// libcurl). Inside a request fiber, the transfer runs on a private curl multi
// handle, and every wait for curl's sockets suspends the fiber while Go waits
// on them (Cairo\Internal\Hooks::waitSet). curl names those sockets through
// its socket callback, never through an fd_set: a busy server's descriptors
// pass FD_SETSIZE (1024), and curl_multi_fdset() silently leaves those out. Everything else in curl_exec()
// stays PHP's own, so curl_getinfo(), curl_error() and the write and header
// callbacks behave as usual. Outside a fiber, the real libcurl function runs.
//
// curl_multi_select() (Guzzle's CurlMultiHandler, parallel curl code) ends in
// curl_multi_wait(), which Cairo defines the same way: inside a request fiber
// the wait for the multi handle's sockets suspends only that fiber.

#ifndef _GNU_SOURCE
#define _GNU_SOURCE
#endif
#include <dlfcn.h>
#include <poll.h>
#include <stdint.h>
#include <stdlib.h>
#include <sys/select.h>

#include <php.h>
#include <curl/curl.h>

#include "curl_hook.h"

// In the static binary (scripts/build-static.sh) libcurl is linked in, so the
// executable cannot simply define curl's functions over the library's. There
// the linker renames calls instead (-Wl,--wrap=curl_easy_perform,
// curl_multi_wait and curl_easy_cleanup): callers reach
// __wrap_curl_easy_perform here, and __real_curl_easy_perform is libcurl's own.
#ifdef CAIRO_STATIC_CURL
#define CAIRO_HOOK(name) __wrap_##name
CURLcode __real_curl_easy_perform(CURL *easy);
CURLMcode __real_curl_multi_wait(CURLM *multi, struct curl_waitfd extra[], unsigned int extra_nfds, int timeout_ms, int *numfds);
void __real_curl_easy_cleanup(CURL *easy);
#else
#define CAIRO_HOOK(name) name
#endif

typedef CURLcode (*easy_perform_fn)(CURL *);

static CURLcode real_easy_perform(CURL *easy)
{
#ifdef CAIRO_STATIC_CURL
	return __real_curl_easy_perform(easy);
#endif
	static easy_perform_fn real = NULL;
	if (real == NULL) {
		real = (easy_perform_fn)dlsym(RTLD_NEXT, "curl_easy_perform");
		if (real == NULL) {
			return CURLE_FAILED_INIT;
		}
	}
	return real(easy);
}

typedef CURLMcode (*multi_wait_fn)(CURLM *, struct curl_waitfd[], unsigned int, int, int *);

static CURLMcode real_multi_wait(CURLM *multi, struct curl_waitfd extra[], unsigned int extra_nfds, int timeout_ms, int *numfds)
{
#ifdef CAIRO_STATIC_CURL
	return __real_curl_multi_wait(multi, extra, extra_nfds, timeout_ms, numfds);
#endif
	static multi_wait_fn real = NULL;
	if (real == NULL) {
		real = (multi_wait_fn)dlsym(RTLD_NEXT, "curl_multi_wait");
		if (real == NULL) {
			return CURLM_INTERNAL_ERROR;
		}
	}
	return real(multi, extra, extra_nfds, timeout_ms, numfds);
}

typedef void (*easy_cleanup_fn)(CURL *);

static void real_easy_cleanup(CURL *easy)
{
#ifdef CAIRO_STATIC_CURL
	__real_curl_easy_cleanup(easy);
	return;
#endif
	static easy_cleanup_fn real = NULL;
	if (real == NULL) {
		real = (easy_cleanup_fn)dlsym(RTLD_NEXT, "curl_easy_cleanup");
		if (real == NULL) {
			return;
		}
	}
	real(easy);
}

// A hooked transfer runs on a multi handle, and a multi handle keeps the
// connections its transfers opened. Each easy handle keeps its own multi
// handle, so the next curl_exec() on it reuses the kept-alive connection,
// as curl_easy_perform() would. It is the handle's until curl_easy_cleanup().
// PHP objects never cross threads, so the table is per thread.
#define CAIRO_MULTI_BUCKETS 256

// A socket curl wants watched, and for what (CURL_POLL_IN, _OUT, _INOUT).
struct curl_sock {
	curl_socket_t fd;
	int what;
};

struct easy_multi {
	CURL *easy;
	CURLM *multi;
	struct curl_sock *socks;   // kept up to date by track_socket()
	int nsocks;
	int capsocks;
	struct easy_multi *next;
};

static __thread struct easy_multi *easy_multis[CAIRO_MULTI_BUCKETS];

// Multi handles whose easy handle was closed, kept with their open
// connections for the thread's next new easy handle. Code that makes a handle
// per request (Laravel's Http client, a new Guzzle client each call) then
// reuses connections as code that keeps one handle does. At most
// CAIRO_SPARE_MULTIS per thread, each with curl's own cap on connections.
#define CAIRO_SPARE_MULTIS 4

static __thread struct easy_multi *spare_multis;
static __thread int nspare;

static void easy_multi_free(struct easy_multi *m)
{
	curl_multi_cleanup(m->multi);
	free(m->socks);
	free(m);
}

// Closes the thread's spare multi handles, when a PHP thread ends.
static void cairo_curl_thread_end(void)
{
	while (spare_multis != NULL) {
		struct easy_multi *m = spare_multis;
		spare_multis = m->next;
		easy_multi_free(m);
	}
	nspare = 0;
}

const cairo_extension cairo_curl_hook = {.thread_end = cairo_curl_thread_end};

static struct easy_multi **easy_multi_slot(CURL *easy)
{
	struct easy_multi **p = &easy_multis[((uintptr_t)easy >> 4) % CAIRO_MULTI_BUCKETS];
	while (*p != NULL && (*p)->easy != easy) {
		p = &(*p)->next;
	}
	return p;
}

// CURLMOPT_SOCKETFUNCTION: curl says which sockets to watch. It is called from
// curl_multi_socket_action(), so the list is current whenever we wait.
static int track_socket(CURL *easy, curl_socket_t fd, int what, void *userp, void *socketp)
{
	(void)easy;
	(void)socketp;
	struct easy_multi *m = userp;
	for (int i = 0; i < m->nsocks; i++) {
		if (m->socks[i].fd == fd) {
			if (what == CURL_POLL_REMOVE) {
				m->socks[i] = m->socks[--m->nsocks];
			} else {
				m->socks[i].what = what;
			}
			return 0;
		}
	}
	if (what == CURL_POLL_REMOVE) {
		return 0;
	}
	if (m->nsocks == m->capsocks) {
		int cap = m->capsocks == 0 ? 4 : m->capsocks * 2;
		struct curl_sock *socks = realloc(m->socks, (size_t)cap * sizeof(*socks));
		if (socks == NULL) {
			return -1;   // curl fails the transfer
		}
		m->socks = socks;
		m->capsocks = cap;
	}
	m->socks[m->nsocks].fd = fd;
	m->socks[m->nsocks].what = what;
	m->nsocks++;
	return 0;
}

static struct easy_multi *easy_multi_get(CURL *easy)
{
	struct easy_multi **p = easy_multi_slot(easy);
	if (*p != NULL) {
		return *p;
	}
	struct easy_multi *m = spare_multis;
	if (m != NULL) {
		spare_multis = m->next;
		nspare--;
		m->easy = easy;
		m->next = NULL;
		*p = m;
		return m;
	}
	m = calloc(1, sizeof(*m));
	if (m == NULL) {
		return NULL;
	}
	m->multi = curl_multi_init();
	if (m->multi == NULL
		|| curl_multi_setopt(m->multi, CURLMOPT_SOCKETFUNCTION, track_socket) != CURLM_OK
		|| curl_multi_setopt(m->multi, CURLMOPT_SOCKETDATA, m) != CURLM_OK) {
		if (m->multi != NULL) {
			curl_multi_cleanup(m->multi);
		}
		free(m);
		return NULL;
	}
	m->easy = easy;
	*p = m;
	return m;
}

void CAIRO_HOOK(curl_easy_cleanup)(CURL *easy)
{
	struct easy_multi **p = easy_multi_slot(easy);
	if (*p != NULL) {
		struct easy_multi *m = *p;
		*p = m->next;
		if (nspare < CAIRO_SPARE_MULTIS) {
			m->easy = NULL;
			m->next = spare_multis;
			spare_multis = m;
			nspare++;
		} else {
			easy_multi_free(m);
		}
	}
	real_easy_cleanup(easy);
}

// Suspends the fiber until a descriptor in read (an array of ints) is
// readable, one in write is writable, or the timeout passes (ms, < 0: none).
// Frees both arrays. Returns 1 when it waited (or timed out), 0 when the
// runtime declined (hooks off, not a Cairo fiber), -1 when an exception was
// thrown.
static int wait_fds(zval *read, zval *write, long timeout_ms)
{
	zval fname, args[3], retval;
	ZVAL_COPY_VALUE(&args[0], read);
	ZVAL_COPY_VALUE(&args[1], write);
	ZVAL_DOUBLE(&args[2], timeout_ms < 0 ? -1.0 : (double)timeout_ms / 1000.0);
	ZVAL_STRING(&fname, "Cairo\\Internal\\Hooks::waitSet");
	ZVAL_UNDEF(&retval);
	int rc = call_user_function(NULL, NULL, &fname, &retval, 3, args);
	zval_ptr_dtor(&fname);
	zval_ptr_dtor(&args[0]);
	zval_ptr_dtor(&args[1]);
	if (EG(exception) != NULL) {
		zval_ptr_dtor(&retval);
		return -1;
	}
	int out = (rc == SUCCESS && Z_TYPE(retval) == IS_LONG) ? (int)Z_LVAL(retval) : 0;
	zval_ptr_dtor(&retval);
	return out != 0 ? 1 : 0;
}

// While curl has no socket (it is resolving a name, or between connection
// attempts), poll again soon rather than wait for a socket that is not there.
static long between_sockets(long timeout_ms)
{
	return timeout_ms < 0 || timeout_ms > 100 ? 100 : timeout_ms;
}

// Waits for the sockets a hooked transfer's own multi handle watches.
static int wait_tracked(struct easy_multi *m, long timeout_ms)
{
	zval read, write;
	array_init(&read);
	array_init(&write);
	for (int i = 0; i < m->nsocks; i++) {
		if (m->socks[i].what & CURL_POLL_IN) {
			add_next_index_long(&read, m->socks[i].fd);
		}
		if (m->socks[i].what & CURL_POLL_OUT) {
			add_next_index_long(&write, m->socks[i].fd);
		}
	}
	return wait_fds(&read, &write, m->nsocks == 0 ? between_sockets(timeout_ms) : timeout_ms);
}

// Lets curl act on each of its sockets that is ready now, then on a timeout
// that is due. Updates *running.
static CURLMcode drive(struct easy_multi *m, int *running)
{
	CURLMcode mc = CURLM_OK;
	// curl edits the list while it acts: walking down, a removal only moves
	// an already visited entry into place, and additions wait for the next round.
	for (int i = m->nsocks - 1; i >= 0 && mc == CURLM_OK; i--) {
		if (i >= m->nsocks) {
			continue;
		}
		struct pollfd p = {.fd = m->socks[i].fd, .events = 0};
		if (m->socks[i].what & CURL_POLL_IN) {
			p.events |= POLLIN;
		}
		if (m->socks[i].what & CURL_POLL_OUT) {
			p.events |= POLLOUT;
		}
		if (poll(&p, 1, 0) <= 0) {
			continue;
		}
		int ev = 0;
		if (p.revents & (POLLIN | POLLHUP)) {
			ev |= CURL_CSELECT_IN;
		}
		if (p.revents & POLLOUT) {
			ev |= CURL_CSELECT_OUT;
		}
		if (p.revents & (POLLERR | POLLNVAL)) {
			ev |= CURL_CSELECT_ERR;
		}
		mc = curl_multi_socket_action(m->multi, p.fd, ev, running);
	}
	long timeout_ms = -1;
	if (mc == CURLM_OK && curl_multi_timeout(m->multi, &timeout_ms) == CURLM_OK && timeout_ms == 0) {
		mc = curl_multi_socket_action(m->multi, CURL_SOCKET_TIMEOUT, 0, running);
	}
	return mc;
}

// Waits for the sockets of a multi handle PHP owns (curl_multi_select()).
// Cairo cannot set a socket callback there, so it asks curl for the
// descriptors: curl_multi_waitfds() (libcurl 8.8) sees all of them, while
// curl_multi_fdset() on older libcurl misses those past FD_SETSIZE.
static int wait_multi(CURLM *multi, long timeout_ms)
{
	zval read, write;
	array_init(&read);
	array_init(&write);
	int any = 0;
#if LIBCURL_VERSION_NUM >= 0x080800
	unsigned int size = 32, n = 0;
	struct curl_waitfd *ufds = NULL;
	CURLMcode mc = CURLM_OUT_OF_MEMORY;
	do {
		size *= 2;
		struct curl_waitfd *more = realloc(ufds, size * sizeof(*ufds));
		if (more == NULL) {
			break;
		}
		ufds = more;
		mc = curl_multi_waitfds(multi, ufds, size, &n);
	} while (mc == CURLM_OUT_OF_MEMORY && size < 1024);
	if (ufds != NULL && mc == CURLM_OK) {
		for (unsigned int i = 0; i < n; i++) {
			if (ufds[i].events & (CURL_WAIT_POLLIN | CURL_WAIT_POLLPRI)) {
				add_next_index_long(&read, ufds[i].fd);
			}
			if (ufds[i].events & CURL_WAIT_POLLOUT) {
				add_next_index_long(&write, ufds[i].fd);
			}
		}
		any = n > 0;
	}
	free(ufds);
#else
	fd_set r, w, e;
	int maxfd = -1;
	FD_ZERO(&r);
	FD_ZERO(&w);
	FD_ZERO(&e);
	if (curl_multi_fdset(multi, &r, &w, &e, &maxfd) == CURLM_OK) {
		for (int fd = 0; fd <= maxfd; fd++) {
			if (FD_ISSET(fd, &r) || FD_ISSET(fd, &e)) {
				add_next_index_long(&read, fd);
			}
			if (FD_ISSET(fd, &w)) {
				add_next_index_long(&write, fd);
			}
		}
		any = maxfd >= 0;
	}
#endif
	return wait_fds(&read, &write, any ? timeout_ms : between_sockets(timeout_ms));
}

CURLcode CAIRO_HOOK(curl_easy_perform)(CURL *easy)
{
	if (EG(active_fiber) == NULL) {
		return real_easy_perform(easy);
	}
	struct easy_multi *m = easy_multi_get(easy);
	if (m == NULL || curl_multi_add_handle(m->multi, easy) != CURLM_OK) {
		return real_easy_perform(easy);
	}

	CURLcode result = CURLE_OK;
	int running = 1;
	int hooked = 1;
	CURLMcode mc = curl_multi_socket_action(m->multi, CURL_SOCKET_TIMEOUT, 0, &running);
	while (mc == CURLM_OK && running) {
		long timeout_ms = -1;
		curl_multi_timeout(m->multi, &timeout_ms);
		if (timeout_ms != 0) {
			int waited = hooked ? wait_tracked(m, timeout_ms) : 0;
			if (waited < 0) {
				// The request's deadline passed (or it was cancelled): stop the
				// transfer; the exception propagates out of curl_exec().
				result = CURLE_ABORTED_BY_CALLBACK;
				break;
			}
			if (waited == 0) {
				hooked = 0;   // declined: wait the blocking way from now on
				int numfds = 0;
				real_multi_wait(m->multi, NULL, 0, timeout_ms < 0 ? 1000 : (int)timeout_ms, &numfds);
			}
		}
		mc = drive(m, &running);
	}
	if (mc != CURLM_OK && result == CURLE_OK) {
		result = CURLE_FAILED_INIT;
	}

	if (result == CURLE_OK) {
		int pending = 0;
		CURLMsg *msg;
		while ((msg = curl_multi_info_read(m->multi, &pending)) != NULL) {
			if (msg->msg == CURLMSG_DONE && msg->easy_handle == easy) {
				result = msg->data.result;
			}
		}
	}
	curl_multi_remove_handle(m->multi, easy);
	return result;
}

CURLMcode CAIRO_HOOK(curl_multi_wait)(CURLM *multi, struct curl_waitfd extra[], unsigned int extra_nfds, int timeout_ms, int *numfds)
{
	// Extra descriptors are not something PHP passes; leave those to curl.
	if (EG(active_fiber) == NULL || extra_nfds != 0) {
		return real_multi_wait(multi, extra, extra_nfds, timeout_ms, numfds);
	}
	// Like curl, never wait past curl's own next timeout.
	long curl_timeout = -1;
	curl_multi_timeout(multi, &curl_timeout);
	long wait_ms = timeout_ms;
	if (curl_timeout >= 0 && (wait_ms < 0 || curl_timeout < wait_ms)) {
		wait_ms = curl_timeout;
	}
	if (wait_ms == 0) {
		return real_multi_wait(multi, NULL, 0, 0, numfds);
	}
	int waited = wait_multi(multi, wait_ms);
	if (waited == 0) {
		return real_multi_wait(multi, extra, extra_nfds, timeout_ms, numfds);
	}
	if (waited < 0) {
		// An exception (deadline, cancellation) is pending; it surfaces as
		// soon as curl_multi_select() returns to PHP.
		if (numfds != NULL) {
			*numfds = 0;
		}
		return CURLM_OK;
	}
	// The fiber woke up because a socket is ready or the time passed: a
	// non-blocking poll tells the caller how many descriptors are ready.
	return real_multi_wait(multi, NULL, 0, 0, numfds);
}
