// The streamed answer of an HTTP request, on its way from PHP to Go.
//
// A piece of the body is one call into Go, which queues it for the
// goroutine that writes to the client (cairo_http_send).
//
// Code that answers with echo (classic PHP in isolated mode, a framework's
// streamed response) has its output taken here, per fiber: what PHP writes
// and no output buffer holds comes to the SAPI module, which asks the
// extensions (output), and a fiber that is being captured keeps it until
// flush() or until it has grown large. Then Cairo\Internal\HttpOutput::drain()
// sends it, and may wait there for a slow client: that is the fiber's wait,
// in the middle of its echo.

#include <limits.h>
#include <stdlib.h>

#include <php.h>
#include <SAPI.h>
#include <zend_fibers.h>
#include <zend_smart_str.h>

#include "extension.h"
#include "http_stream.h"
#include "_cgo_export.h"

// What a captured fiber holds before it is sent.
#define DRAIN_AT 8192

typedef struct {
	smart_str held;
	bool draining;
} capture;

// The captures of this thread, by fiber: memory of the thread's PHP request.
static __thread HashTable *captures;

static capture *capture_of_fiber(void)
{
	if (captures == NULL || EG(active_fiber) == NULL) {
		return NULL;
	}
	return zend_hash_index_find_ptr(captures, (zend_ulong)(uintptr_t)EG(active_fiber));
}

static void drain(capture *c)
{
	if (c->draining) {
		return;
	}
	c->draining = true;
	zval fn, ret;
	ZVAL_STRING(&fn, "Cairo\\Internal\\HttpOutput::drain");
	ZVAL_UNDEF(&ret);
	call_user_function(NULL, NULL, &fn, &ret, 0, NULL);
	zval_ptr_dtor(&fn);
	zval_ptr_dtor(&ret);
	// The fiber may have waited in there, but only it ends its capture.
	c->draining = false;
}

static bool cairo_http_output(const char *str, size_t len)
{
	capture *c = capture_of_fiber();
	if (c == NULL) {
		return false;
	}
	smart_str_appendl(&c->held, str, len);
	if (ZSTR_LEN(c->held.s) >= DRAIN_AT) {
		drain(c);
	}
	return true;
}

static void cairo_http_flush(void)
{
	capture *c = capture_of_fiber();
	if (c != NULL) {
		drain(c);
	}
}

static void take(capture *c, zval *return_value)
{
	if (c == NULL || c->held.s == NULL) {
		RETURN_EMPTY_STRING();
	}
	RETVAL_STR(smart_str_extract(&c->held));
}

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_http_capture, 0, 0, 1)
	ZEND_ARG_INFO(0, on)
ZEND_END_ARG_INFO()

// cairo_http_capture(bool $on): string begins or ends taking the output of
// the running fiber, and returns what it still held. PHP's own note that
// headers were sent ends with it, in both directions: the thread's PHP
// request never ends, and the next answer has headers of its own.
static PHP_FUNCTION(cairo_http_capture)
{
	bool on;
	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_BOOL(on)
	ZEND_PARSE_PARAMETERS_END();
	if (EG(active_fiber) == NULL) {
		zend_throw_error(NULL, "cairo_http_capture(): no fiber is running");
		RETURN_THROWS();
	}
	SG(headers_sent) = 0;
	zend_ulong key = (zend_ulong)(uintptr_t)EG(active_fiber);
	capture *c = capture_of_fiber();
	if (!on) {
		take(c, return_value);
		if (c != NULL) {
			zend_hash_index_del(captures, key);
			efree(c);
		}
		return;
	}
	if (c == NULL) {
		if (captures == NULL) {
			ALLOC_HASHTABLE(captures);
			zend_hash_init(captures, 8, NULL, NULL, 0);
		}
		c = ecalloc(1, sizeof(capture));
		zend_hash_index_add_new_ptr(captures, key, c);
	}
	RETURN_EMPTY_STRING();
}

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_http_captured, 0, 0, 0)
ZEND_END_ARG_INFO()

// cairo_http_captured(): string takes what the running fiber's capture holds.
static PHP_FUNCTION(cairo_http_captured)
{
	ZEND_PARSE_PARAMETERS_NONE();
	take(capture_of_fiber(), return_value);
}

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_http_send, 0, 0, 2)
	ZEND_ARG_INFO(0, stream)
	ZEND_ARG_INFO(0, bytes)
ZEND_END_ARG_INFO()

// cairo_http_send(int $stream, string $bytes): bool|int queues a piece of a
// streamed answer's body: true when it is queued, false when the queue is
// full (the Go operation http.stream.write then waits for room), -1 when
// the client is gone and -2 when the stream has ended.
static PHP_FUNCTION(cairo_http_send)
{
	zend_long stream;
	char *bytes;
	size_t len;
	ZEND_PARSE_PARAMETERS_START(2, 2)
		Z_PARAM_LONG(stream)
		Z_PARAM_STRING(bytes, len)
	ZEND_PARSE_PARAMETERS_END();
	if (len > INT_MAX) {
		zend_value_error("cairo_http_send(): the piece is larger than 2 GB");
		RETURN_THROWS();
	}
	int queued = goHTTPSend(stream, bytes, (int)len);
	if (queued < 0) {
		RETURN_LONG(queued);
	}
	RETURN_BOOL(queued);
}

static const zend_function_entry cairo_http_stream_functions[] = {
	PHP_FE(cairo_http_capture, arginfo_cairo_http_capture)
	PHP_FE(cairo_http_captured, arginfo_cairo_http_captured)
	PHP_FE(cairo_http_send, arginfo_cairo_http_send)
	PHP_FE_END
};

const cairo_extension cairo_http_stream = {
	.output = cairo_http_output,
	.flush = cairo_http_flush,
	.functions = cairo_http_stream_functions,
};
