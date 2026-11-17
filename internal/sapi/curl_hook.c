// Cairo's curl hook.
//
// PHP's curl_exec() ends in curl_easy_perform(), which blocks the thread while
// libcurl polls its sockets. Cairo defines curl_easy_perform in the binary, so
// libphp's call resolves here first (the executable is searched before
// libcurl). Inside a request fiber, the transfer runs on a private curl multi
// handle, and every wait for curl's sockets suspends the fiber while Go waits
// on them (Cairo\Internal\Hooks::waitSet). Everything else in curl_exec()
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
#include <stdint.h>
#include <stdlib.h>
#include <sys/select.h>

#include <php.h>
#include <curl/curl.h>

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
// as curl_easy_perform() would. It lives until curl_easy_cleanup(). PHP
// objects never cross threads, so the table is per thread.
#define CAIRO_MULTI_BUCKETS 256

struct easy_multi {
	CURL *easy;
	CURLM *multi;
	struct easy_multi *next;
};

static __thread struct easy_multi *easy_multis[CAIRO_MULTI_BUCKETS];

static struct easy_multi **easy_multi_slot(CURL *easy)
{
	struct easy_multi **p = &easy_multis[((uintptr_t)easy >> 4) % CAIRO_MULTI_BUCKETS];
	while (*p != NULL && (*p)->easy != easy) {
		p = &(*p)->next;
	}
	return p;
}

static CURLM *easy_multi_get(CURL *easy)
{
	struct easy_multi **p = easy_multi_slot(easy);
	if (*p != NULL) {
		return (*p)->multi;
	}
	struct easy_multi *m = malloc(sizeof(*m));
	if (m == NULL) {
		return NULL;
	}
	m->multi = curl_multi_init();
	if (m->multi == NULL) {
		free(m);
		return NULL;
	}
	m->easy = easy;
	m->next = NULL;
	*p = m;
	return m->multi;
}

void CAIRO_HOOK(curl_easy_cleanup)(CURL *easy)
{
	struct easy_multi **p = easy_multi_slot(easy);
	if (*p != NULL) {
		struct easy_multi *m = *p;
		*p = m->next;
		curl_multi_cleanup(m->multi);
		free(m);
	}
	real_easy_cleanup(easy);
}

// Waits until one of curl's sockets is ready or the timeout passes.
// Returns 1 when it waited (or timed out), 0 when the runtime declined
// (hooks off, not a Cairo fiber), -1 when an exception was thrown.
static int wait_curl_sockets(CURLM *multi, long timeout_ms)
{
	fd_set r, w, e;
	int maxfd = -1;
	FD_ZERO(&r);
	FD_ZERO(&w);
	FD_ZERO(&e);
	if (curl_multi_fdset(multi, &r, &w, &e, &maxfd) != CURLM_OK) {
		return 0;
	}
	if (maxfd == -1 && (timeout_ms < 0 || timeout_ms > 100)) {
		timeout_ms = 100;   // curl is between sockets (e.g. resolving): poll again soon
	}

	zval fname, args[3], retval;
	array_init(&args[0]);
	array_init(&args[1]);
	for (int fd = 0; fd <= maxfd; fd++) {
		if (FD_ISSET(fd, &r) || FD_ISSET(fd, &e)) {
			add_next_index_long(&args[0], fd);
		}
		if (FD_ISSET(fd, &w)) {
			add_next_index_long(&args[1], fd);
		}
	}
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

CURLcode CAIRO_HOOK(curl_easy_perform)(CURL *easy)
{
	if (EG(active_fiber) == NULL) {
		return real_easy_perform(easy);
	}
	CURLM *multi = easy_multi_get(easy);
	if (multi == NULL || curl_multi_add_handle(multi, easy) != CURLM_OK) {
		return real_easy_perform(easy);
	}

	CURLcode result = CURLE_OK;
	int running = 1;
	int hooked = 1;
	while (running) {
		CURLMcode mc = curl_multi_perform(multi, &running);
		if (mc != CURLM_OK) {
			result = CURLE_FAILED_INIT;
			break;
		}
		if (!running) {
			break;
		}
		long timeout_ms = -1;
		curl_multi_timeout(multi, &timeout_ms);
		if (timeout_ms == 0) {
			continue;
		}
		int waited = hooked ? wait_curl_sockets(multi, timeout_ms) : 0;
		if (waited < 0) {
			// The request's deadline passed (or it was cancelled): stop the
			// transfer; the exception propagates out of curl_exec().
			result = CURLE_ABORTED_BY_CALLBACK;
			running = 0;
			break;
		}
		if (waited == 0) {
			hooked = 0;   // declined: wait the blocking way from now on
			int numfds = 0;
			real_multi_wait(multi, NULL, 0, timeout_ms < 0 ? 1000 : (int)timeout_ms, &numfds);
		}
	}

	if (result == CURLE_OK) {
		int pending = 0;
		CURLMsg *msg;
		while ((msg = curl_multi_info_read(multi, &pending)) != NULL) {
			if (msg->msg == CURLMSG_DONE && msg->easy_handle == easy) {
				result = msg->data.result;
			}
		}
	}
	curl_multi_remove_handle(multi, easy);
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
	int waited = wait_curl_sockets(multi, wait_ms);
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
