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

#ifndef _GNU_SOURCE
#define _GNU_SOURCE
#endif
#include <dlfcn.h>
#include <sys/select.h>

#include <php.h>
#include <curl/curl.h>

typedef CURLcode (*easy_perform_fn)(CURL *);

static CURLcode real_easy_perform(CURL *easy)
{
	static easy_perform_fn real = NULL;
	if (real == NULL) {
		real = (easy_perform_fn)dlsym(RTLD_NEXT, "curl_easy_perform");
		if (real == NULL) {
			return CURLE_FAILED_INIT;
		}
	}
	return real(easy);
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

CURLcode curl_easy_perform(CURL *easy)
{
	if (EG(active_fiber) == NULL) {
		return real_easy_perform(easy);
	}
	CURLM *multi = curl_multi_init();
	if (multi == NULL) {
		return real_easy_perform(easy);
	}
	if (curl_multi_add_handle(multi, easy) != CURLM_OK) {
		curl_multi_cleanup(multi);
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
			curl_multi_wait(multi, NULL, 0, timeout_ms < 0 ? 1000 : (int)timeout_ms, &numfds);
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
	curl_multi_cleanup(multi);
	return result;
}
