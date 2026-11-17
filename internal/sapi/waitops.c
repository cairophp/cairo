// The socket waits (see waitops.go): same contract as cairo_fiber_op(), with
// the fields passed as arguments instead of JSON.

#include <php.h>

#include "_cgo_export.h"

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
