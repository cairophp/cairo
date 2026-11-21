// What a fiber asks Go for (see waitops.go, and submit in operation.go).
// Each of these takes the request the fiber belongs to and the scope of its
// context (Context::goScope()), and returns the operation's id, or -1 when
// Go refused it; the fiber then suspends until the operation's result comes
// as an event.

#include <poll.h>

#include <php.h>

#include "extension.h"
#include "loop.h"
#include "wire.h"
#include "_cgo_export.h"

// Descriptors per curl wait; curl uses a handful per transfer.
#define CAIRO_WAITSET_MAX 1024

// cairo_fiber_wait(int $request, float $scope, int $fd, bool $write, float $timeout): int
PHP_FUNCTION(cairo_fiber_wait)
{
	zend_long request, fd;
	bool write;
	double scope, timeout;
	ZEND_PARSE_PARAMETERS_START(5, 5)
		Z_PARAM_LONG(request)
		Z_PARAM_DOUBLE(scope)
		Z_PARAM_LONG(fd)
		Z_PARAM_BOOL(write)
		Z_PARAM_DOUBLE(timeout)
	ZEND_PARSE_PARAMETERS_END();
	if (fd < 0 || fd > INT_MAX) {
		RETURN_LONG(-1);
	}
	RETURN_LONG((zend_long)goFiberWait(cairo_thread_id(), (long long)request, scope, (int)fd, write ? 1 : 0, timeout));
}

// Copies a list of descriptors into out; returns its length, or -1 when an
// entry is not a valid descriptor or there are too many.
static int cairo_fd_list(HashTable *ht, int *out)
{
	uint32_t n = zend_hash_num_elements(ht);
	if (n > CAIRO_WAITSET_MAX) {
		return -1;
	}
	int i = 0;
	zval *v;
	ZEND_HASH_FOREACH_VAL(ht, v) {
		if (Z_TYPE_P(v) != IS_LONG || Z_LVAL_P(v) < 0 || Z_LVAL_P(v) > INT_MAX) {
			return -1;
		}
		out[i++] = (int)Z_LVAL_P(v);
	} ZEND_HASH_FOREACH_END();
	return i;
}

// cairo_fiber_waitset(int $request, float $scope, list<int> $read, list<int> $write, float $timeout): int
PHP_FUNCTION(cairo_fiber_waitset)
{
	zend_long request;
	HashTable *read, *write;
	double scope, timeout;
	ZEND_PARSE_PARAMETERS_START(5, 5)
		Z_PARAM_LONG(request)
		Z_PARAM_DOUBLE(scope)
		Z_PARAM_ARRAY_HT(read)
		Z_PARAM_ARRAY_HT(write)
		Z_PARAM_DOUBLE(timeout)
	ZEND_PARSE_PARAMETERS_END();
	int *fds = emalloc(sizeof(int) * 2 * CAIRO_WAITSET_MAX);
	int nr = cairo_fd_list(read, fds);
	int nw = nr < 0 ? -1 : cairo_fd_list(write, fds + CAIRO_WAITSET_MAX);
	zend_long id = -1;
	if (nw >= 0) {
		id = (zend_long)goFiberWaitSet(cairo_thread_id(), (long long)request, scope, fds, nr, fds + CAIRO_WAITSET_MAX, nw, timeout);
	}
	efree(fds);
	RETURN_LONG(id);
}

// cairo_fiber_call(int $request, float $scope, string $op, string|array $payload): int
// An array is the arguments of a Go operation, which cross in internal/wire's
// form (wire.Unmarshal reads them); a string crosses as it is.
PHP_FUNCTION(cairo_fiber_call)
{
	zend_long request;
	double scope;
	zend_string *name, *payload;
	HashTable *args;
	ZEND_PARSE_PARAMETERS_START(4, 4)
		Z_PARAM_LONG(request)
		Z_PARAM_DOUBLE(scope)
		Z_PARAM_STR(name)
		Z_PARAM_ARRAY_HT_OR_STR(args, payload)
	ZEND_PARSE_PARAMETERS_END();
	smart_str encoded = {0};
	if (args != NULL) {
		if (cairo_wire_encode(args, &encoded) == FAILURE) {
			smart_str_free(&encoded);
			RETURN_THROWS();
		}
		payload = encoded.s;
	}
	zend_long id = -1;
	if (ZSTR_LEN(name) <= INT_MAX && ZSTR_LEN(payload) <= INT_MAX) {
		id = (zend_long)goFiberCall(cairo_thread_id(), (long long)request, scope, ZSTR_VAL(name), (int)ZSTR_LEN(name),
			ZSTR_VAL(payload), (int)ZSTR_LEN(payload));
	}
	smart_str_free(&encoded);
	RETURN_LONG(id);
}

// cairo_op_cancel(int $id): void
// Ends the context of an operation that this thread submitted with a scope,
// when PHP cancels a context it runs under. The operation still reports,
// with the context's error.
PHP_FUNCTION(cairo_op_cancel)
{
	zend_long id;
	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_LONG(id)
	ZEND_PARSE_PARAMETERS_END();
	goOpCancel(cairo_thread_id(), (long long)id);
}

// Longest wait on the PHP thread itself, in milliseconds: long enough for a
// database on the same network to answer, short enough that a cancelled
// request or a new one is not held up noticeably.
#define CAIRO_WAIT_HERE_MS 1

// cairo_wait_here(int $fd, bool $write, bool $unlessQueued): bool
// Waits for the socket on this thread, without Go, and says whether it became
// ready. A wait through Go costs two thread switches each way; a reply that
// comes within the millisecond costs one poll() here. The scheduler calls it
// only when no other fiber could run meanwhile (Scheduler::waitHere), and with
// $unlessQueued it declines when Go already has an event for this thread.
PHP_FUNCTION(cairo_wait_here)
{
	zend_long fd;
	bool write, unless_queued;
	ZEND_PARSE_PARAMETERS_START(3, 3)
		Z_PARAM_LONG(fd)
		Z_PARAM_BOOL(write)
		Z_PARAM_BOOL(unless_queued)
	ZEND_PARSE_PARAMETERS_END();
	if (fd < 0 || fd > INT_MAX || (unless_queued && cairo_loop_has_event())) {
		RETURN_FALSE;
	}
	struct pollfd pfd = {.fd = (int)fd, .events = write ? POLLOUT : POLLIN};
	RETURN_BOOL(poll(&pfd, 1, CAIRO_WAIT_HERE_MS) > 0);
}
