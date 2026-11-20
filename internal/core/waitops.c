// The most frequent fiber operations (see waitops.go): same contract as cairo_fiber_op(), with
// the fields passed as arguments instead of JSON.

#include <poll.h>

#include <php.h>

#include "_cgo_export.h"

int cairo_thread_id(void);   // cairo.c

// Descriptors per curl wait; curl uses a handful per transfer.
#define CAIRO_WAITSET_MAX 1024

// cairo_fiber_wait(int $fiber, int $fd, bool $write, float $timeout): int
PHP_FUNCTION(cairo_fiber_wait)
{
	zend_long fiber, fd;
	bool write;
	double timeout;
	ZEND_PARSE_PARAMETERS_START(4, 4)
		Z_PARAM_LONG(fiber)
		Z_PARAM_LONG(fd)
		Z_PARAM_BOOL(write)
		Z_PARAM_DOUBLE(timeout)
	ZEND_PARSE_PARAMETERS_END();
	if (fd < 0 || fd > INT_MAX) {
		RETURN_LONG(-1);
	}
	RETURN_LONG((zend_long)goFiberWait((long long)fiber, (int)fd, write ? 1 : 0, timeout));
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

// cairo_fiber_waitset(int $fiber, list<int> $read, list<int> $write, float $timeout): int
PHP_FUNCTION(cairo_fiber_waitset)
{
	zend_long fiber;
	HashTable *read, *write;
	double timeout;
	ZEND_PARSE_PARAMETERS_START(4, 4)
		Z_PARAM_LONG(fiber)
		Z_PARAM_ARRAY_HT(read)
		Z_PARAM_ARRAY_HT(write)
		Z_PARAM_DOUBLE(timeout)
	ZEND_PARSE_PARAMETERS_END();
	int *fds = emalloc(sizeof(int) * 2 * CAIRO_WAITSET_MAX);
	int nr = cairo_fd_list(read, fds);
	int nw = nr < 0 ? -1 : cairo_fd_list(write, fds + CAIRO_WAITSET_MAX);
	zend_long id = -1;
	if (nw >= 0) {
		id = (zend_long)goFiberWaitSet((long long)fiber, fds, nr, fds + CAIRO_WAITSET_MAX, nw, timeout);
	}
	efree(fds);
	RETURN_LONG(id);
}

// cairo_fiber_call(int $fiber, string $op, string $payload): int
PHP_FUNCTION(cairo_fiber_call)
{
	zend_long fiber;
	zend_string *name, *payload;
	ZEND_PARSE_PARAMETERS_START(3, 3)
		Z_PARAM_LONG(fiber)
		Z_PARAM_STR(name)
		Z_PARAM_STR(payload)
	ZEND_PARSE_PARAMETERS_END();
	if (ZSTR_LEN(name) > INT_MAX || ZSTR_LEN(payload) > INT_MAX) {
		RETURN_LONG(-1);
	}
	RETURN_LONG((zend_long)goFiberCall((long long)fiber, ZSTR_VAL(name), (int)ZSTR_LEN(name),
		ZSTR_VAL(payload), (int)ZSTR_LEN(payload)));
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
	if (fd < 0 || fd > INT_MAX || (unless_queued && goInboxLen(cairo_thread_id()) > 0)) {
		RETURN_FALSE;
	}
	struct pollfd pfd = {.fd = (int)fd, .events = write ? POLLOUT : POLLIN};
	RETURN_BOOL(poll(&pfd, 1, CAIRO_WAIT_HERE_MS) > 0);
}
