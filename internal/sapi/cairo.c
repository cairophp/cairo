// Cairo's bridge between Go and an embedded, thread-safe (ZTS) PHP.
//
// Cairo is its own PHP SAPI ("cairo", below): one boot thread runs the
// declare phase, then each PHP worker thread gets its own interpreter context
// (ts_resource) and runs the Cairo runtime script, which loops on
// cairo_next_event() and runs every request in its own Fiber.

#include <pthread.h>
#include <stdatomic.h>
#include <stdlib.h>
#include <string.h>

#include <php.h>
#include <php_main.h>
#include <main/build-defs.h>
#include <SAPI.h>
#include <TSRM.h>
#include <zend_exceptions.h>
#include <zend_fibers.h>
#include <zend_observer.h>
#include <zend_extensions.h>
#include <zend_stream.h>
#include <php_network.h>
#include <ext/json/php_json.h>
#include <zend_smart_str.h>
#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <unistd.h>
#include <php_variables.h>
#include <signal.h>
#include <ext/standard/php_fopen_wrappers.h>

#include "cairo.h"
#include "pgsql.h"
#include "_cgo_export.h"

// -1 is the boot thread (declare phase); 0..n-1 are worker threads.
static __thread int cairo_tid = -1;

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_none, 0, 0, 0)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_spec, 0, 0, 1)
	ZEND_ARG_INFO(0, json)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_respond, 0, 0, 2)
	ZEND_ARG_INFO(0, id)
	ZEND_ARG_INFO(0, json)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_fiber_open, 0, 0, 1)
	ZEND_ARG_INFO(0, parent)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_fiber_close, 0, 0, 1)
	ZEND_ARG_INFO(0, fiber)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_fiber_op, 0, 0, 2)
	ZEND_ARG_INFO(0, fiber)
	ZEND_ARG_INFO(0, json)
ZEND_END_ARG_INFO()

// cairo_info(): string (JSON: mode, server file, thread id)
PHP_FUNCTION(cairo_info)
{
	ZEND_PARSE_PARAMETERS_NONE();
	char *s = goCairoInfo(cairo_tid);
	RETVAL_STRING(s);
	free(s);
}

// cairo_spec(string $json): void. Declare phase: records the spec.
// Serve phase: checks this thread built the same spec as the boot thread.
PHP_FUNCTION(cairo_spec)
{
	char *json;
	size_t len;
	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_STRING(json, len)
	ZEND_PARSE_PARAMETERS_END();
	char *err = goCairoSpec(cairo_tid, json, (int)len);
	if (err != NULL) {
		zend_throw_exception(NULL, err, 0);
		free(err);
	}
}

// cairo_next_event(): string (JSON). Blocks this PHP thread until Go has
// work for it: a new request, a finished I/O operation, or shutdown.
PHP_FUNCTION(cairo_next_event)
{
	ZEND_PARSE_PARAMETERS_NONE();
	char *s = goNextEvent(cairo_tid);
	RETVAL_STRING(s);
	free(s);
}

// cairo_respond(int $id, string $json): void
PHP_FUNCTION(cairo_respond)
{
	zend_long id;
	char *json;
	size_t len;
	ZEND_PARSE_PARAMETERS_START(2, 2)
		Z_PARAM_LONG(id)
		Z_PARAM_STRING(json, len)
	ZEND_PARSE_PARAMETERS_END();
	goRespond((long long)id, json, (int)len);
}

// cairo_retire(string $reason): void. Asks Go to replace this request thread
// with a fresh one; Go then sends a "retire" event, after which the thread
// finishes the requests it holds and exits.
PHP_FUNCTION(cairo_retire)
{
	char *reason;
	size_t len;
	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_STRING(reason, len)
	ZEND_PARSE_PARAMETERS_END();
	goThreadRetire(cairo_tid, reason, (int)len);
}

// cairo_wake(int $op): void. Wakes a fiber parked by a "park" operation
// (Cairo\Channel, select()); harmless if it already woke.
PHP_FUNCTION(cairo_wake)
{
	zend_long op;
	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_LONG(op)
	ZEND_PARSE_PARAMETERS_END();
	goFiberWake((long long)op);
}

// cairo_stream_fd(resource $stream): int is the descriptor under a socket or
// pipe stream, or -1 when it has none. Cairo\Revolt\Driver waits on it.
PHP_FUNCTION(cairo_stream_fd)
{
	zval *zstream;
	php_stream *stream;
	php_socket_t fd = -1;
	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_RESOURCE(zstream)
	ZEND_PARSE_PARAMETERS_END();
	php_stream_from_zval(stream, zstream);
	int as = PHP_STREAM_AS_FD_FOR_SELECT | PHP_STREAM_CAST_INTERNAL;
	if (php_stream_can_cast(stream, as) == SUCCESS
		&& php_stream_cast(stream, as, (void **)&fd, 0) == SUCCESS && fd >= 0) {
		RETURN_LONG((zend_long)fd);
	}
	RETURN_LONG(-1);
}

// cairo_fiber_open(int $parentFiber): int. Gives a new fiber its own
// goroutine, with a child of the parent fiber's context.
PHP_FUNCTION(cairo_fiber_open)
{
	zend_long parent;
	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_LONG(parent)
	ZEND_PARSE_PARAMETERS_END();
	RETURN_LONG((zend_long)goFiberOpen(cairo_tid, (long long)parent));
}

// cairo_fiber_close(int $fiber): void
PHP_FUNCTION(cairo_fiber_close)
{
	zend_long fiber;
	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_LONG(fiber)
	ZEND_PARSE_PARAMETERS_END();
	goFiberClose((long long)fiber);
}

// cairo_context_open(int $parent, float $timeout): int. A derived context
// (Context::withTimeout(), withCancel()) with its own goroutine, like a fiber's;
// $timeout < 0 means none.
PHP_FUNCTION(cairo_context_open)
{
	zend_long parent;
	double timeout;
	ZEND_PARSE_PARAMETERS_START(2, 2)
		Z_PARAM_LONG(parent)
		Z_PARAM_DOUBLE(timeout)
	ZEND_PARSE_PARAMETERS_END();
	RETURN_LONG((zend_long)goContextOpen(cairo_tid, (long long)parent, timeout));
}

// cairo_context_cancel(int $id): void. Cancels a context; its goroutine stays
// until cairo_fiber_close(), answering operations with the error.
PHP_FUNCTION(cairo_context_cancel)
{
	zend_long id;
	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_LONG(id)
	ZEND_PARSE_PARAMETERS_END();
	goContextCancel((long long)id);
}

// cairo_fiber_op(int $fiber, string $json): int. Asks the fiber's goroutine
// to perform an operation; returns its id (or -1). The caller then suspends.
PHP_FUNCTION(cairo_fiber_op)
{
	zend_long fiber;
	char *json;
	size_t len;
	ZEND_PARSE_PARAMETERS_START(2, 2)
		Z_PARAM_LONG(fiber)
		Z_PARAM_STRING(json, len)
	ZEND_PARSE_PARAMETERS_END();
	RETURN_LONG((zend_long)goFiberOp((long long)fiber, json, (int)len));
}

// cairo_go_call(string $op, string $json): string. Runs a Go operation on
// this thread, outside a fiber; returns {"result": ...} or {"error": ...}.
ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_go_call, 0, 0, 2)
	ZEND_ARG_INFO(0, op)
	ZEND_ARG_INFO(0, json)
ZEND_END_ARG_INFO()

PHP_FUNCTION(cairo_go_call)
{
	zend_string *name, *json;
	ZEND_PARSE_PARAMETERS_START(2, 2)
		Z_PARAM_STR(name)
		Z_PARAM_STR(json)
	ZEND_PARSE_PARAMETERS_END();
	char *res = goCall(ZSTR_VAL(name), ZSTR_VAL(json), (int)ZSTR_LEN(json));
	RETVAL_STRING(res);
	free(res);
}

PHP_FUNCTION(cairo_guard);

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_retire, 0, 0, 1)
	ZEND_ARG_INFO(0, reason)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_wake, 0, 0, 1)
	ZEND_ARG_INFO(0, op)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_stream_fd, 0, 0, 1)
	ZEND_ARG_INFO(0, stream)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_context_open, 0, 0, 2)
	ZEND_ARG_INFO(0, parent)
	ZEND_ARG_INFO(0, timeout)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_guard, 0, 0, 1)
	ZEND_ARG_INFO(0, fn)
ZEND_END_ARG_INFO()

static const zend_function_entry cairo_functions[] = {
	PHP_FE(cairo_info, arginfo_cairo_none)
	PHP_FE(cairo_spec, arginfo_cairo_spec)
	PHP_FE(cairo_next_event, arginfo_cairo_none)
	PHP_FE(cairo_respond, arginfo_cairo_respond)
	PHP_FE(cairo_wake, arginfo_cairo_wake)
	PHP_FE(cairo_stream_fd, arginfo_cairo_stream_fd)
	PHP_FE(cairo_retire, arginfo_cairo_retire)
	PHP_FE(cairo_fiber_open, arginfo_cairo_fiber_open)
	PHP_FE(cairo_fiber_close, arginfo_cairo_fiber_close)
	PHP_FE(cairo_context_open, arginfo_cairo_context_open)
	PHP_FE(cairo_context_cancel, arginfo_cairo_fiber_close)
	PHP_FE(cairo_fiber_op, arginfo_cairo_fiber_op)
	PHP_FE(cairo_go_call, arginfo_cairo_go_call)
	PHP_FE(cairo_guard, arginfo_cairo_guard)
	PHP_FE_END
};

// ---- Runtime hooks -------------------------------------------------------
//
// Cairo swaps the handlers of PHP's blocking sleep functions. Inside a fiber,
// the new handler asks the PHP runtime (Cairo\Internal\Hooks::sleep) to
// suspend the fiber while its goroutine waits; PHP fibers are stackful, so
// the fiber can suspend from inside this C function. Outside a fiber, or when
// the runtime declines (hooks off, offload thread, a fiber Cairo did not
// start), the original handler runs and blocks as usual.

enum {
	HOOK_SLEEP, HOOK_USLEEP, HOOK_NANOSLEEP, HOOK_GETHOSTBYNAME, HOOK_GETHOSTBYNAMEL,
	HOOK_IS_UPLOADED_FILE, HOOK_MOVE_UPLOADED_FILE, HOOK_COUNT
};

static const char *hook_names[HOOK_COUNT] = {
	"sleep", "usleep", "time_nanosleep", "gethostbyname", "gethostbynamel",
	"is_uploaded_file", "move_uploaded_file",
};
static zif_handler hook_originals[HOOK_COUNT];

// Returns 1 when the runtime slept for us (or threw), 0 to fall back.
static int cairo_hooked_sleep(double seconds)
{
	if (EG(active_fiber) == NULL) {
		return 0;
	}
	zval fname, arg, retval;
	ZVAL_STRING(&fname, "Cairo\\Internal\\Hooks::sleep");
	ZVAL_DOUBLE(&arg, seconds);
	ZVAL_UNDEF(&retval);
	int rc = call_user_function(NULL, NULL, &fname, &retval, 1, &arg);
	zval_ptr_dtor(&fname);
	if (rc != SUCCESS) {
		return EG(exception) != NULL;
	}
	if (EG(exception) != NULL) {
		zval_ptr_dtor(&retval);
		return 1;
	}
	int handled = zend_is_true(&retval);
	zval_ptr_dtor(&retval);
	return handled;
}

static ZEND_NAMED_FUNCTION(cairo_hook_sleep)
{
	zend_long seconds;
	if (ZEND_NUM_ARGS() == 1 && Z_TYPE_P(ZEND_CALL_ARG(execute_data, 1)) == IS_LONG) {
		seconds = Z_LVAL_P(ZEND_CALL_ARG(execute_data, 1));
		if (seconds >= 0 && cairo_hooked_sleep((double)seconds)) {
			if (EG(exception) == NULL) {
				RETVAL_LONG(0);
			}
			return;
		}
	}
	hook_originals[HOOK_SLEEP](INTERNAL_FUNCTION_PARAM_PASSTHRU);
}

static ZEND_NAMED_FUNCTION(cairo_hook_usleep)
{
	if (ZEND_NUM_ARGS() == 1 && Z_TYPE_P(ZEND_CALL_ARG(execute_data, 1)) == IS_LONG) {
		zend_long us = Z_LVAL_P(ZEND_CALL_ARG(execute_data, 1));
		if (us >= 0 && cairo_hooked_sleep((double)us / 1e6)) {
			return;
		}
	}
	hook_originals[HOOK_USLEEP](INTERNAL_FUNCTION_PARAM_PASSTHRU);
}

static ZEND_NAMED_FUNCTION(cairo_hook_nanosleep)
{
	if (ZEND_NUM_ARGS() == 2
		&& Z_TYPE_P(ZEND_CALL_ARG(execute_data, 1)) == IS_LONG
		&& Z_TYPE_P(ZEND_CALL_ARG(execute_data, 2)) == IS_LONG) {
		zend_long s = Z_LVAL_P(ZEND_CALL_ARG(execute_data, 1));
		zend_long ns = Z_LVAL_P(ZEND_CALL_ARG(execute_data, 2));
		if (s >= 0 && ns >= 0 && ns <= 999999999 && cairo_hooked_sleep((double)s + (double)ns / 1e9)) {
			if (EG(exception) == NULL) {
				RETVAL_TRUE;
			}
			return;
		}
	}
	hook_originals[HOOK_NANOSLEEP](INTERNAL_FUNCTION_PARAM_PASSTHRU);
}

// Forwards a call to Cairo\Internal\Hooks::<name>(...$args). A null result
// means the runtime declined, and the original function runs instead.
static int cairo_forward(const char *method, zend_execute_data *execute_data, zval *return_value)
{
	if (EG(active_fiber) == NULL) {
		return 0;
	}
	char name[96];
	snprintf(name, sizeof(name), "Cairo\\Internal\\Hooks::%s", method);
	zval fname, retval;
	ZVAL_STRING(&fname, name);
	ZVAL_UNDEF(&retval);
	uint32_t argc = ZEND_NUM_ARGS();
	int rc = call_user_function(NULL, NULL, &fname, &retval, argc, argc ? ZEND_CALL_ARG(execute_data, 1) : NULL);
	zval_ptr_dtor(&fname);
	if (EG(exception) != NULL) {
		zval_ptr_dtor(&retval);
		return 1;
	}
	if (rc != SUCCESS || Z_TYPE(retval) == IS_NULL || Z_TYPE(retval) == IS_UNDEF) {
		zval_ptr_dtor(&retval);
		return 0;
	}
	ZVAL_COPY_VALUE(return_value, &retval);
	return 1;
}

static ZEND_NAMED_FUNCTION(cairo_hook_gethostbyname)
{
	if (!cairo_forward("gethostbyname", execute_data, return_value)) {
		hook_originals[HOOK_GETHOSTBYNAME](INTERNAL_FUNCTION_PARAM_PASSTHRU);
	}
}

static ZEND_NAMED_FUNCTION(cairo_hook_gethostbynamel)
{
	if (!cairo_forward("gethostbynamel", execute_data, return_value)) {
		hook_originals[HOOK_GETHOSTBYNAMEL](INTERNAL_FUNCTION_PARAM_PASSTHRU);
	}
}

// Uploads are parsed by Go, so PHP's own list of uploaded files is empty;
// these ask the runtime about Cairo's uploads first.
static ZEND_NAMED_FUNCTION(cairo_hook_is_uploaded_file)
{
	if (!cairo_forward("isUploadedFile", execute_data, return_value)) {
		hook_originals[HOOK_IS_UPLOADED_FILE](INTERNAL_FUNCTION_PARAM_PASSTHRU);
	}
}

static ZEND_NAMED_FUNCTION(cairo_hook_move_uploaded_file)
{
	if (!cairo_forward("moveUploadedFile", execute_data, return_value)) {
		hook_originals[HOOK_MOVE_UPLOADED_FILE](INTERNAL_FUNCTION_PARAM_PASSTHRU);
	}
}

static const zif_handler hook_handlers[HOOK_COUNT] = {
	cairo_hook_sleep, cairo_hook_usleep, cairo_hook_nanosleep,
	cairo_hook_gethostbyname, cairo_hook_gethostbynamel,
	cairo_hook_is_uploaded_file, cairo_hook_move_uploaded_file,
};


// ---- Socket hooks ----------------------------------------------------------
//
// Cairo re-registers the tcp and unix stream transports. Streams are still
// created by PHP's own socket code, but get a copy of its ops whose read and
// write first wait for the descriptor through Go (epoll), suspending only the
// calling fiber. Everything built on PHP streams benefits: mysqlnd (PDO MySQL,
// mysqli), Predis and phpredis, fsockopen(), stream_socket_client(). The
// connect itself still blocks for now.

// What a hooked ops table is for. TLS streams are plain sockets to Go, which
// does the TLS; the kind also carries the protocol version the transport
// name asked for (tlsv1.2:// and so on).
enum {
	SOCK_TCP, SOCK_UNIX, SOCK_TLS, SOCK_TLS10, SOCK_TLS11, SOCK_TLS12, SOCK_TLS13, SOCK_KINDS
};

#define CAIRO_MAX_SOCK_OPS 16
static const php_stream_ops *sock_ops_orig[CAIRO_MAX_SOCK_OPS];
static php_stream_ops sock_ops_hooked[CAIRO_MAX_SOCK_OPS];
static int sock_ops_kind[CAIRO_MAX_SOCK_OPS];
static int sock_ops_count;

static const php_stream_ops *find_orig_ops(const php_stream_ops *hooked)
{
	// Entries never move once added, so a hooked pointer maps to its index.
	for (int i = 0; i < CAIRO_MAX_SOCK_OPS; i++) {
		if (&sock_ops_hooked[i] == hooked) {
			return sock_ops_orig[i];
		}
	}
	return NULL;
}

// Asks the runtime to wait (Cairo\Internal\Hooks::wait). Returns 1 when the
// socket is ready, 0 when the runtime declined (block as usual), -1 on the
// socket's own timeout, -2 when an exception was thrown (deadline).
static int cairo_wait_fd(php_netstream_data_t *sock, int write)
{
	if (EG(active_fiber) == NULL) {
		return 0;
	}
	struct pollfd pfd = {.fd = sock->socket, .events = write ? POLLOUT : POLLIN};
	if (poll(&pfd, 1, 0) > 0) {
		return 1;   // ready now: no need to suspend
	}
	double timeout = -1;
	if (sock->timeout.tv_sec != -1) {
		timeout = (double)sock->timeout.tv_sec + (double)sock->timeout.tv_usec / 1e6;
	}
	zval fname, args[3], retval;
	ZVAL_STRING(&fname, "Cairo\\Internal\\Hooks::wait");
	ZVAL_LONG(&args[0], (zend_long)sock->socket);
	ZVAL_BOOL(&args[1], write);
	ZVAL_DOUBLE(&args[2], timeout);
	ZVAL_UNDEF(&retval);
	int rc = call_user_function(NULL, NULL, &fname, &retval, 3, args);
	zval_ptr_dtor(&fname);
	if (EG(exception) != NULL) {
		zval_ptr_dtor(&retval);
		return -2;
	}
	if (rc != SUCCESS) {
		return 0;
	}
	int out = Z_TYPE(retval) == IS_LONG ? (int)Z_LVAL(retval) : 0;
	zval_ptr_dtor(&retval);
	return out;
}

// TLS options from the stream context's "ssl" group, in Go's terms.
static void cairo_tls_options(php_stream *stream, int kind, zval *out)
{
	array_init(out);
	add_assoc_long(out, "version", kind == SOCK_TLS10 ? 10 : kind == SOCK_TLS11 ? 11 : kind == SOCK_TLS12 ? 12 : kind == SOCK_TLS13 ? 13 : 0);
	php_stream_context *ctx = PHP_STREAM_CONTEXT(stream);
	const char *bools[] = {"verify_peer", "verify_peer_name", "allow_self_signed"};
	const int defaults[] = {1, 1, 0};
	for (int i = 0; i < 3; i++) {
		zval *v = ctx ? php_stream_context_get_option(ctx, "ssl", bools[i]) : NULL;
		add_assoc_bool(out, bools[i], v ? zend_is_true(v) : defaults[i]);
	}
	const char *strs[] = {"peer_name", "cafile", "capath", "local_cert", "local_pk", "passphrase", "SNI_server_name"};
	for (int i = 0; i < 7; i++) {
		zval *v = ctx ? php_stream_context_get_option(ctx, "ssl", strs[i]) : NULL;
		if (v != NULL && Z_TYPE_P(v) == IS_STRING) {
			add_assoc_str(out, strs[i], zend_string_copy(Z_STR_P(v)));
		}
	}
}

// Applies a connect result: a descriptor (>= 0) or an error message.
static void cairo_connect_result(php_netstream_data_t *sock, php_stream_xport_param *xparam, zval *result)
{
	if (Z_TYPE_P(result) == IS_LONG && Z_LVAL_P(result) >= 0) {
		sock->socket = (php_socket_t)Z_LVAL_P(result);
		xparam->outputs.returncode = 0;
		xparam->outputs.error_code = 0;
		return;
	}
	xparam->outputs.returncode = -1;
	xparam->outputs.error_code = ECONNREFUSED;
	if (xparam->want_errortext) {
		xparam->outputs.error_text = Z_TYPE_P(result) == IS_STRING
			? zend_string_copy(Z_STR_P(result))
			: zend_string_init("connect failed", sizeof("connect failed") - 1, 0);
	}
}

// Connects a TLS stream without a fiber: Go dials and handshakes while this
// thread waits.
static void cairo_tls_connect_sync(php_netstream_data_t *sock, php_stream_xport_param *xparam, double timeout, zval *opts)
{
	smart_str json = {0};
	zval req;
	array_init(&req);
	add_assoc_stringl(&req, "address", xparam->inputs.name, xparam->inputs.namelen);
	add_assoc_double(&req, "timeout", timeout);
	Z_ADDREF_P(opts);
	add_assoc_zval(&req, "tls", opts);
	php_json_encode(&json, &req, 0);
	zval_ptr_dtor(&req);
	smart_str_0(&json);
	char *errmsg = NULL;
	int fd = goDialTLS(ZSTR_VAL(json.s), (int)ZSTR_LEN(json.s), &errmsg);
	smart_str_free(&json);
	zval result;
	if (fd >= 0) {
		ZVAL_LONG(&result, fd);
	} else {
		ZVAL_STRING(&result, errmsg != NULL ? errmsg : "TLS connect failed");
	}
	free(errmsg);
	cairo_connect_result(sock, xparam, &result);
	zval_ptr_dtor(&result);
}

// Connects through Go (Cairo\Internal\Hooks::connect): the name lookup, the
// connect and, for TLS, the handshake run on the fiber's goroutine with Go's
// resolver and netpoller. Returns 1 when handled (xparam filled in), 0 to let
// PHP connect itself. TLS streams are always handled: PHP only sees a plain
// socket to Go, which encrypts and decrypts.
static int cairo_sock_connect(php_stream *stream, php_netstream_data_t *sock, php_stream_xport_param *xparam, int kind)
{
	int is_tls = kind >= SOCK_TLS;
	if (sock == NULL || sock->socket != -1 || xparam->inputs.name == NULL) {
		return 0;
	}
	double timeout = -1;
	if (xparam->inputs.timeout != NULL) {
		timeout = (double)xparam->inputs.timeout->tv_sec + (double)xparam->inputs.timeout->tv_usec / 1e6;
	}
	zval opts;
	ZVAL_UNDEF(&opts);
	if (is_tls) {
		cairo_tls_options(stream, kind, &opts);
	}
	// Options Go's dialer does not take (a local bind address): let PHP do it.
	if (!is_tls && (EG(active_fiber) == NULL || (kind == SOCK_TCP && PHP_STREAM_CONTEXT(stream)
		&& php_stream_context_get_option(PHP_STREAM_CONTEXT(stream), "socket", "bindto") != NULL))) {
		return 0;
	}
	if (EG(active_fiber) == NULL) {
		cairo_tls_connect_sync(sock, xparam, timeout, &opts);
		zval_ptr_dtor(&opts);
		return 1;
	}
	zval fname, args[4], retval;
	ZVAL_STRING(&fname, "Cairo\\Internal\\Hooks::connect");
	ZVAL_STRING(&args[0], is_tls ? "tls" : kind == SOCK_UNIX ? "unix" : "tcp");
	ZVAL_STRINGL(&args[1], xparam->inputs.name, xparam->inputs.namelen);
	ZVAL_DOUBLE(&args[2], timeout);
	if (is_tls) {
		ZVAL_COPY(&args[3], &opts);
	} else {
		array_init(&args[3]);
	}
	ZVAL_UNDEF(&retval);
	int rc = call_user_function(NULL, NULL, &fname, &retval, 4, args);
	zval_ptr_dtor(&fname);
	zval_ptr_dtor(&args[0]);
	zval_ptr_dtor(&args[1]);
	zval_ptr_dtor(&args[3]);
	if (EG(exception) != NULL) {
		zval_ptr_dtor(&retval);
		zval_ptr_dtor(&opts);
		xparam->outputs.returncode = -1;
		return 1;
	}
	if (rc != SUCCESS || Z_TYPE(retval) == IS_NULL || Z_TYPE(retval) == IS_UNDEF) {
		zval_ptr_dtor(&retval);
		if (is_tls) {
			// Hooks are off: still TLS through Go, just blocking.
			cairo_tls_connect_sync(sock, xparam, timeout, &opts);
			zval_ptr_dtor(&opts);
			return 1;
		}
		return 0;
	}
	zval_ptr_dtor(&opts);
	cairo_connect_result(sock, xparam, &retval);
	zval_ptr_dtor(&retval);
	return 1;
}

// ---- STARTTLS: stream_socket_enable_crypto() on a plain socket ----------
//
// Go handshakes over its own copy of the socket and hands back a socket pair
// end that carries plaintext; dup2() puts it in place of the stream's
// descriptor, so the PHP stream (and its buffers, timeouts and hooks) stays
// the same object. Only client-side TLS is offered; turning TLS off again is
// not supported.

// The method passed to STREAM_XPORT_CRYPTO_OP_SETUP, for the ENABLE call that
// stream_socket_enable_crypto() makes right after it on the same thread.
static __thread php_stream *crypto_setup_stream;
static __thread int crypto_setup_method;

// The peer's host name, from the stream's "host:port" or "tcp://host:port".
static zend_string *crypto_peer_host(php_stream *stream)
{
	const char *p = stream->orig_path != NULL ? stream->orig_path : "";
	const char *scheme = strstr(p, "://");
	if (scheme != NULL) {
		p = scheme + 3;
	}
	if (*p == '[') {
		const char *end = strchr(p, ']');
		return end != NULL ? zend_string_init(p + 1, end - p - 1, 0) : zend_string_init(p, strlen(p), 0);
	}
	const char *colon = strrchr(p, ':');
	return zend_string_init(p, colon != NULL ? (size_t)(colon - p) : strlen(p), 0);
}

static int cairo_sock_enable_crypto(php_stream *stream, php_netstream_data_t *sock, int method)
{
	if (!(method & 1)) {   // the client bit of STREAM_CRYPTO_METHOD_*
		php_error_docref(NULL, E_WARNING, "Cairo supports TLS only as a client");
		return -1;
	}
	int version = 0;
	switch (method) {
		case STREAM_CRYPTO_METHOD_TLSv1_0_CLIENT: version = SOCK_TLS10; break;
		case STREAM_CRYPTO_METHOD_TLSv1_1_CLIENT: version = SOCK_TLS11; break;
		case STREAM_CRYPTO_METHOD_TLSv1_2_CLIENT: version = SOCK_TLS12; break;
		case STREAM_CRYPTO_METHOD_TLSv1_3_CLIENT: version = SOCK_TLS13; break;
		default: version = SOCK_TLS; break;
	}
	zval opts;
	cairo_tls_options(stream, version, &opts);
	zend_string *host = crypto_peer_host(stream);
	double timeout = -1;
	if (sock->timeout.tv_sec != -1) {
		timeout = (double)sock->timeout.tv_sec + (double)sock->timeout.tv_usec / 1e6;
	}
	int fd = sock->socket;
	int fl = fcntl(fd, F_GETFL);
	int fdfl = fcntl(fd, F_GETFD);

	zval result;
	ZVAL_UNDEF(&result);
	if (EG(active_fiber) != NULL) {
		zval fname, args[4];
		ZVAL_STRING(&fname, "Cairo\\Internal\\Hooks::startTLS");
		ZVAL_LONG(&args[0], fd);
		ZVAL_STR_COPY(&args[1], host);
		ZVAL_DOUBLE(&args[2], timeout);
		ZVAL_COPY(&args[3], &opts);
		int rc = call_user_function(NULL, NULL, &fname, &result, 4, args);
		zval_ptr_dtor(&fname);
		zval_ptr_dtor(&args[1]);
		zval_ptr_dtor(&args[3]);
		if (rc != SUCCESS) {
			zval_ptr_dtor(&result);
			ZVAL_UNDEF(&result);
		}
	}
	if (EG(exception) == NULL && (Z_TYPE(result) == IS_UNDEF || Z_TYPE(result) == IS_NULL)) {
		zval req;
		array_init(&req);
		add_assoc_str(&req, "host", zend_string_copy(host));
		add_assoc_double(&req, "timeout", timeout);
		Z_ADDREF(opts);
		add_assoc_zval(&req, "tls", &opts);
		smart_str json = {0};
		php_json_encode(&json, &req, 0);
		smart_str_0(&json);
		zval_ptr_dtor(&req);
		char *errmsg = NULL;
		int nfd = goStartTLS(fd, ZSTR_VAL(json.s), (int)ZSTR_LEN(json.s), &errmsg);
		smart_str_free(&json);
		if (nfd >= 0) {
			ZVAL_LONG(&result, nfd);
		} else {
			ZVAL_STRING(&result, errmsg != NULL ? errmsg : "TLS handshake failed");
		}
		free(errmsg);
	}
	zend_string_release(host);
	zval_ptr_dtor(&opts);

	int out = -1;
	if (Z_TYPE(result) == IS_LONG && Z_LVAL(result) >= 0) {
		int nfd = (int)Z_LVAL(result);
		if (dup2(nfd, fd) >= 0) {
			out = 1;
		} else {
			php_error_docref(NULL, E_WARNING, "SSL: could not swap the socket: %s", strerror(errno));
		}
		close(nfd);
	} else if (Z_TYPE(result) == IS_STRING) {
		php_error_docref(NULL, E_WARNING, "SSL operation failed: %s", Z_STRVAL(result));
	}
	// Go set the shared socket non-blocking; give the stream's descriptor back
	// its own flags.
	if (fl >= 0) {
		fcntl(fd, F_SETFL, fl);
	}
	if (fdfl >= 0) {
		fcntl(fd, F_SETFD, fdfl);
	}
	zval_ptr_dtor(&result);
	return out;
}

// Appends bytes to the stream's read buffer, ahead of what the socket gives
// next.
static void cairo_stream_unread(php_stream *stream, const char *buf, size_t n)
{
	if (stream->readpos > 0) {
		memmove(stream->readbuf, stream->readbuf + stream->readpos, stream->writepos - stream->readpos);
		stream->writepos -= stream->readpos;
		stream->readpos = 0;
	}
	if (stream->readbuflen - stream->writepos < n) {
		stream->readbuflen = stream->writepos + n;
		stream->readbuf = perealloc(stream->readbuf, stream->readbuflen, stream->is_persistent);
	}
	memcpy(stream->readbuf + stream->writepos, buf, n);
	stream->writepos += n;
}

// stream_socket_enable_crypto($s, false): Go sends what PHP wrote and a
// close_notify, and passes on what the peer sent up to its close_notify;
// that is read from the old descriptor into the stream's buffer, and the
// plain connection takes the descriptor's place.
static int cairo_sock_disable_crypto(php_stream *stream, php_netstream_data_t *sock)
{
	double timeout = -1;
	if (sock->timeout.tv_sec != -1) {
		timeout = (double)sock->timeout.tv_sec + (double)sock->timeout.tv_usec / 1e6;
	}
	int fd = sock->socket;
	int fl = fcntl(fd, F_GETFL);
	int fdfl = fcntl(fd, F_GETFD);

	zval result;
	ZVAL_UNDEF(&result);
	if (EG(active_fiber) != NULL) {
		zval fname, args[2];
		ZVAL_STRING(&fname, "Cairo\\Internal\\Hooks::stopTLS");
		ZVAL_LONG(&args[0], fd);
		ZVAL_DOUBLE(&args[1], timeout);
		int rc = call_user_function(NULL, NULL, &fname, &result, 2, args);
		zval_ptr_dtor(&fname);
		if (rc != SUCCESS) {
			zval_ptr_dtor(&result);
			ZVAL_UNDEF(&result);
		}
	}
	if (EG(exception) == NULL && (Z_TYPE(result) == IS_UNDEF || Z_TYPE(result) == IS_NULL)) {
		char *errmsg = NULL;
		int nfd = goStopTLS(fd, timeout, &errmsg);
		if (nfd >= 0) {
			ZVAL_LONG(&result, nfd);
		} else {
			ZVAL_STRING(&result, errmsg != NULL ? errmsg : "TLS shutdown failed");
		}
		free(errmsg);
	}

	int out = -1;
	if (Z_TYPE(result) == IS_LONG && Z_LVAL(result) >= 0) {
		int nfd = (int)Z_LVAL(result);
		// Go has closed its end of the pair: read what it left until EOF.
		fcntl(fd, F_SETFL, (fl >= 0 ? fl : 0) | O_NONBLOCK);
		char buf[8192];
		for (;;) {
			ssize_t n = read(fd, buf, sizeof(buf));
			if (n > 0) {
				cairo_stream_unread(stream, buf, (size_t)n);
				continue;
			}
			if (n < 0 && errno == EINTR) {
				continue;
			}
			break;
		}
		if (dup2(nfd, fd) >= 0) {
			out = 1;
		} else {
			php_error_docref(NULL, E_WARNING, "SSL: could not swap the socket: %s", strerror(errno));
		}
		close(nfd);
	} else if (Z_TYPE(result) == IS_STRING) {
		php_error_docref(NULL, E_WARNING, "SSL: could not turn TLS off: %s", Z_STRVAL(result));
	}
	if (fl >= 0) {
		fcntl(fd, F_SETFL, fl);
	}
	if (fdfl >= 0) {
		fcntl(fd, F_SETFD, fdfl);
	}
	zval_ptr_dtor(&result);
	return out;
}

static int cairo_sock_set_option(php_stream *stream, int option, int value, void *ptrparam)
{
	const php_stream_ops *orig = find_orig_ops(stream->ops);
	if (option == PHP_STREAM_OPTION_CRYPTO_API && ptrparam != NULL) {
		php_stream_xport_crypto_param *cp = (php_stream_xport_crypto_param *)ptrparam;
		php_netstream_data_t *sock = (php_netstream_data_t *)stream->abstract;
		if (cp->op == STREAM_XPORT_CRYPTO_OP_SETUP) {
			crypto_setup_stream = stream;
			crypto_setup_method = (int)cp->inputs.method;
			cp->outputs.returncode = 0;
			return PHP_STREAM_OPTION_RETURN_OK;
		}
		if (!cp->inputs.activate) {
			if (sock == NULL || sock->socket == -1) {
				cp->outputs.returncode = -1;
				return PHP_STREAM_OPTION_RETURN_OK;
			}
			crypto_setup_stream = NULL;
			cp->outputs.returncode = cairo_sock_disable_crypto(stream, sock);
			return PHP_STREAM_OPTION_RETURN_OK;
		}
		if (crypto_setup_stream != stream || sock == NULL || sock->socket == -1) {
			php_error_docref(NULL, E_WARNING, "SSL/TLS was not set up on this stream");
			cp->outputs.returncode = -1;
			return PHP_STREAM_OPTION_RETURN_OK;
		}
		crypto_setup_stream = NULL;
		cp->outputs.returncode = cairo_sock_enable_crypto(stream, sock, crypto_setup_method);
		return PHP_STREAM_OPTION_RETURN_OK;
	}
	if (option == PHP_STREAM_OPTION_XPORT_API && ptrparam != NULL) {
		php_stream_xport_param *xparam = (php_stream_xport_param *)ptrparam;
		if (xparam->op == STREAM_XPORT_OP_CONNECT) {
			int kind = SOCK_TCP;
			for (int i = 0; i < CAIRO_MAX_SOCK_OPS; i++) {
				if (&sock_ops_hooked[i] == stream->ops) {
					kind = sock_ops_kind[i];
				}
			}
			if (cairo_sock_connect(stream, (php_netstream_data_t *)stream->abstract, xparam, kind)) {
				return PHP_STREAM_OPTION_RETURN_OK;
			}
		}
	}
	return orig->set_option(stream, option, value, ptrparam);
}

static ssize_t cairo_sock_read(php_stream *stream, char *buf, size_t count)
{
	const php_stream_ops *orig = find_orig_ops(stream->ops);
	php_netstream_data_t *sock = (php_netstream_data_t *)stream->abstract;
	if (sock != NULL && sock->socket != -1 && sock->is_blocked && !stream->has_buffered_data
		&& !(sock->timeout.tv_sec == 0 && sock->timeout.tv_usec == 0)) {
		int r = cairo_wait_fd(sock, 0);
		if (r == -1) {
			sock->timeout_event = 1;
			return -1;
		}
		if (r == -2) {
			return -1;
		}
	}
	return orig->read(stream, buf, count);
}

static ssize_t cairo_sock_write(php_stream *stream, const char *buf, size_t count)
{
	const php_stream_ops *orig = find_orig_ops(stream->ops);
	php_netstream_data_t *sock = (php_netstream_data_t *)stream->abstract;
	if (sock != NULL && sock->socket != -1 && sock->is_blocked) {
		int r = cairo_wait_fd(sock, 1);
		if (r == -1) {
			sock->timeout_event = 1;
			return -1;
		}
		if (r == -2) {
			return -1;
		}
	}
	return orig->write(stream, buf, count);
}

// The hooked copy of a socket ops table, made the first time a stream with
// that table is created. The table only grows, under a mutex.
static pthread_mutex_t sock_ops_mu = PTHREAD_MUTEX_INITIALIZER;

static const php_stream_ops *hooked_ops_for(const php_stream_ops *ops, int kind)
{
	const php_stream_ops *out = NULL;
	pthread_mutex_lock(&sock_ops_mu);
	for (int i = 0; i < sock_ops_count; i++) {
		if (sock_ops_orig[i] == ops && sock_ops_kind[i] == kind) {
			out = &sock_ops_hooked[i];
			break;
		}
	}
	if (out == NULL && sock_ops_count < CAIRO_MAX_SOCK_OPS && ops->read != NULL && ops->write != NULL) {
		int i = sock_ops_count;
		sock_ops_orig[i] = ops;
		sock_ops_hooked[i] = *ops;
		sock_ops_hooked[i].read = cairo_sock_read;
		sock_ops_hooked[i].write = cairo_sock_write;
		if (ops->set_option != NULL) {
			sock_ops_hooked[i].set_option = cairo_sock_set_option;
		}
		sock_ops_kind[i] = kind;
		sock_ops_count++;
		out = &sock_ops_hooked[i];
	}
	pthread_mutex_unlock(&sock_ops_mu);
	return out;
}

static int sock_kind(const char *proto, size_t protolen)
{
	static const struct { const char *name; int kind; } names[] = {
		{"tcp", SOCK_TCP}, {"unix", SOCK_UNIX}, {"ssl", SOCK_TLS}, {"tls", SOCK_TLS},
		{"tlsv1.0", SOCK_TLS10}, {"tlsv1.1", SOCK_TLS11}, {"tlsv1.2", SOCK_TLS12}, {"tlsv1.3", SOCK_TLS13},
	};
	for (size_t i = 0; i < sizeof(names) / sizeof(names[0]); i++) {
		if (strlen(names[i].name) == protolen && strncasecmp(proto, names[i].name, protolen) == 0) {
			return names[i].kind;
		}
	}
	return SOCK_TCP;
}

// ext/openssl's own factories, when PHP has it: used outside Cairo fibers.
static php_stream_transport_factory tls_factory_orig[SOCK_KINDS];

static php_stream *cairo_socket_factory(const char *proto, size_t protolen,
		const char *resourcename, size_t resourcenamelen,
		const char *persistent_id, int options, int flags,
		struct timeval *timeout,
		php_stream_context *context STREAMS_DC)
{
	int kind = sock_kind(proto, protolen);
	if (kind >= SOCK_TLS && tls_factory_orig[kind] != NULL && EG(active_fiber) == NULL) {
		return tls_factory_orig[kind](proto, protolen, resourcename, resourcenamelen,
			persistent_id, options, flags, timeout, context STREAMS_REL_CC);
	}
	// TLS streams start as plain TCP sockets; connecting goes through Go.
	php_stream *stream = php_stream_generic_socket_factory(kind >= SOCK_TLS ? "tcp" : proto, kind >= SOCK_TLS ? 3 : protolen,
		resourcename, resourcenamelen, persistent_id, options, flags, timeout, context STREAMS_REL_CC);
	if (stream != NULL) {
		const php_stream_ops *hooked = hooked_ops_for(stream->ops, kind);
		if (hooked != NULL) {
			stream->ops = hooked;
		}
	}
	return stream;
}

static void cairo_hook_sockets(void)
{
	php_stream_xport_register("tcp", cairo_socket_factory);
	php_stream_xport_register("unix", cairo_socket_factory);

	// TLS through Go: works without ext/openssl, and suspends only the fiber.
	static const char *tls_names[] = {"ssl", "tls", "tlsv1.0", "tlsv1.1", "tlsv1.2", "tlsv1.3"};
	HashTable *xports = php_stream_xport_get_hash();
	for (size_t i = 0; i < sizeof(tls_names) / sizeof(tls_names[0]); i++) {
		int kind = sock_kind(tls_names[i], strlen(tls_names[i]));
		php_stream_transport_factory orig = zend_hash_str_find_ptr(xports, tls_names[i], strlen(tls_names[i]));
		if (orig != NULL && tls_factory_orig[kind] == NULL) {
			tls_factory_orig[kind] = orig;
		}
		php_stream_xport_register(tls_names[i], cairo_socket_factory);
	}
	// Without ext/openssl PHP has no https:// wrapper; PHP's http wrapper
	// works over Cairo's ssl:// transport.
	if (zend_hash_str_find(php_stream_get_url_stream_wrappers_hash_global(), "https", 5) == NULL) {
		php_register_url_stream_wrapper("https", &php_stream_http_wrapper);
	}
}

// ---- Per-fiber engine state -----------------------------------------------
//
// Some PHP code switches the engine's error mode for the length of a call:
// PDO's constructor turns warnings into PDOException while it connects. With
// hooks, that call can suspend its fiber, and the mode, a per-thread global,
// would leak into whichever fiber runs next (and be "restored" wrongly
// later). Cairo keeps the mode per fiber and swaps it on every fiber switch.

typedef struct {
	zend_error_handling_t error_handling;
	zend_class_entry *exception_class;
	int guards;    // cairo_guard() calls running in this fiber
	bool stopped;  // Context\DeadlineExceeded was thrown here at the deadline
	bool unwound;  // then an unwind (exit) that cairo_guard() turns back into it
} cairo_fiber_state;

static int cairo_fiber_slot = -1;

static void cairo_fiber_switch(zend_fiber_context *from, zend_fiber_context *to)
{
	cairo_fiber_state *st = from->reserved[cairo_fiber_slot];
	if (from->status == ZEND_FIBER_STATUS_DEAD) {
		free(st);
		from->reserved[cairo_fiber_slot] = NULL;
	} else {
		if (st == NULL) {
			st = calloc(1, sizeof(*st));
			from->reserved[cairo_fiber_slot] = st;
		}
		if (st != NULL) {
			st->error_handling = EG(error_handling);
			st->exception_class = EG(exception_class);
		}
	}
	cairo_fiber_state *next = to->reserved[cairo_fiber_slot];
	if (next != NULL) {
		EG(error_handling) = next->error_handling;
		EG(exception_class) = next->exception_class;
	} else if (to->status == ZEND_FIBER_STATUS_INIT) {
		// A new fiber starts with PHP's normal error handling.
		EG(error_handling) = EH_NORMAL;
		EG(exception_class) = NULL;
	}
}

// --- Hard stop at the deadline ---------------------------------------------
//
// Code that never suspends its fiber (a CPU loop) cannot see the deadline
// through Go. When a request's deadline passes, Go calls cairo_interrupt()
// for its thread every few milliseconds until PHP answers. It raises the
// thread's VM interrupt flag, and at the next safe point (a loop back-edge or
// a call) the engine runs cairo_on_interrupt in whatever fiber is running.
// Cairo\Internal\interrupt() says whether that fiber's request is past its
// deadline, not yet answered, and running user code (not Cairo's runtime).
// If so, the first time Context\DeadlineExceeded is thrown, so the code can
// clean up. If the code catches it and carries on, the next time the fiber is
// unwound the way exit() does it (catch and finally blocks are skipped,
// destructors run), and cairo_guard(), around the handler and each async()
// task, turns the unwind back into Context\DeadlineExceeded for the runtime.

#define CAIRO_MAX_THREADS 4096

typedef struct {
#if PHP_VERSION_ID >= 80200
	_Atomic(zend_atomic_bool *) vm_interrupt;
#else
	_Atomic(volatile zend_bool *) vm_interrupt;
#endif
	atomic_bool poked;
	atomic_int blocked_ms;   // > 0: report that the running code blocked the thread this long
} cairo_thread_slot;

static cairo_thread_slot cairo_slots[CAIRO_MAX_THREADS];
static void (*cairo_prev_interrupt)(zend_execute_data *execute_data);

static void cairo_interrupt_register(void)
{
	if (cairo_tid >= 0 && cairo_tid < CAIRO_MAX_THREADS) {
		atomic_store(&cairo_slots[cairo_tid].poked, false);
		atomic_store(&cairo_slots[cairo_tid].vm_interrupt, &EG(vm_interrupt));
	}
}

static void cairo_interrupt_unregister(void)
{
	if (cairo_tid >= 0 && cairo_tid < CAIRO_MAX_THREADS) {
		atomic_store(&cairo_slots[cairo_tid].vm_interrupt, NULL);
	}
}

// Called from any goroutine.
void cairo_interrupt(int tid)
{
	if (tid < 0 || tid >= CAIRO_MAX_THREADS) {
		return;
	}
	cairo_thread_slot *slot = &cairo_slots[tid];
	atomic_store(&slot->poked, true);
#if PHP_VERSION_ID >= 80200
	zend_atomic_bool *flag = atomic_load(&slot->vm_interrupt);
	if (flag != NULL) {
		zend_atomic_bool_store(flag, true);
	}
#else
	volatile zend_bool *flag = atomic_load(&slot->vm_interrupt);
	if (flag != NULL) {
		*flag = 1;
	}
#endif
}

// Called from Go's blocking detector (App::detectBlocking()): the thread has
// run PHP for ms milliseconds without waiting for an event. At the next safe
// point the running fiber logs where it is.
void cairo_report_blocking(int tid, int ms)
{
	if (tid < 0 || tid >= CAIRO_MAX_THREADS) {
		return;
	}
	cairo_thread_slot *slot = &cairo_slots[tid];
	atomic_store(&slot->blocked_ms, ms);
#if PHP_VERSION_ID >= 80200
	zend_atomic_bool *flag = atomic_load(&slot->vm_interrupt);
	if (flag != NULL) {
		zend_atomic_bool_store(flag, true);
	}
#else
	volatile zend_bool *flag = atomic_load(&slot->vm_interrupt);
	if (flag != NULL) {
		*flag = 1;
	}
#endif
}

static void cairo_call_report_blocking(int ms)
{
	zval fn, ret, arg;
	ZVAL_STRING(&fn, "Cairo\\Internal\\reportBlocking");
	ZVAL_LONG(&arg, ms);
	ZVAL_UNDEF(&ret);
	call_user_function(NULL, NULL, &fn, &ret, 1, &arg);
	zval_ptr_dtor(&fn);
	zval_ptr_dtor(&ret);
}

// The running fiber's Cairo state, created when missing.
static cairo_fiber_state *cairo_current_fiber_state(void)
{
	if (cairo_fiber_slot < 0 || EG(active_fiber) == NULL) {
		return NULL;
	}
	zend_fiber_context *ctx = &EG(active_fiber)->context;
	cairo_fiber_state *st = ctx->reserved[cairo_fiber_slot];
	if (st == NULL) {
		st = calloc(1, sizeof(*st));
		if (st != NULL) {
			st->error_handling = EG(error_handling);
			st->exception_class = EG(exception_class);
		}
		ctx->reserved[cairo_fiber_slot] = st;
	}
	return st;
}

static void cairo_throw_deadline(void)
{
	zend_string *name = zend_string_init("Cairo\\Context\\DeadlineExceeded", sizeof("Cairo\\Context\\DeadlineExceeded") - 1, 0);
	zend_class_entry *ce = zend_lookup_class(name);
	zend_string_release(name);
	zend_throw_exception(ce != NULL ? ce : zend_ce_exception, "context deadline exceeded", 0);
}

// Whether the running fiber should stop: Cairo\Internal\interrupt($file).
static bool cairo_should_stop(void)
{
	zend_string *file = zend_get_executed_filename_ex();
	zval fn, ret, arg;
	ZVAL_STRING(&fn, "Cairo\\Internal\\interrupt");
	if (file != NULL) {
		ZVAL_STR_COPY(&arg, file);
	} else {
		ZVAL_NULL(&arg);
	}
	ZVAL_UNDEF(&ret);
	bool stop = call_user_function(NULL, NULL, &fn, &ret, 1, &arg) == SUCCESS
		&& EG(exception) == NULL && Z_TYPE(ret) == IS_TRUE;
	zval_ptr_dtor(&fn);
	zval_ptr_dtor(&arg);
	zval_ptr_dtor(&ret);
	return stop;
}

static void cairo_on_interrupt(zend_execute_data *execute_data)
{
	if (cairo_tid >= 0 && cairo_tid < CAIRO_MAX_THREADS && EG(active_fiber) != NULL && EG(exception) == NULL) {
		int ms = atomic_exchange(&cairo_slots[cairo_tid].blocked_ms, 0);
		if (ms > 0) {
			cairo_call_report_blocking(ms);
		}
	}
	if (cairo_tid >= 0 && cairo_tid < CAIRO_MAX_THREADS
		&& EG(active_fiber) != NULL && EG(exception) == NULL
		&& atomic_exchange(&cairo_slots[cairo_tid].poked, false)) {
		cairo_fiber_state *st = cairo_current_fiber_state();
		if (st != NULL && cairo_should_stop()) {
			if (st->stopped && st->guards > 0) {
				st->unwound = true;
				zend_throw_unwind_exit();
			} else {
				st->stopped = true;
				cairo_throw_deadline();
			}
		}
	}
	if (cairo_prev_interrupt != NULL) {
		cairo_prev_interrupt(execute_data);
	}
}

// cairo_guard(callable $fn): mixed calls $fn; an unwind started at the
// deadline ends here and becomes Context\DeadlineExceeded.
PHP_FUNCTION(cairo_guard)
{
	zend_fcall_info fci;
	zend_fcall_info_cache fcc;
	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_FUNC(fci, fcc)
	ZEND_PARSE_PARAMETERS_END();
	cairo_fiber_state *st = cairo_current_fiber_state();
	if (st != NULL) {
		st->guards++;
	}
	fci.retval = return_value;
	zend_call_function(&fci, &fcc);
	if (st != NULL) {
		st->guards--;
		if (st->guards == 0 && st->unwound && EG(exception) != NULL && zend_is_unwind_exit(EG(exception))) {
			st->unwound = false;
			zend_clear_exception();
			zval_ptr_dtor(return_value);
			ZVAL_NULL(return_value);
			cairo_throw_deadline();
		}
	}
}

// Module startup: install the hooks once, before any worker thread copies
// the function table.
static void cairo_opcache_disguise(void);

static PHP_MINIT_FUNCTION(cairo)
{
	cairo_opcache_disguise();
	for (int i = 0; i < HOOK_COUNT; i++) {
		zend_function *fn = zend_hash_str_find_ptr(CG(function_table), hook_names[i], strlen(hook_names[i]));
		if (fn != NULL && fn->type == ZEND_INTERNAL_FUNCTION) {
			hook_originals[i] = fn->internal_function.handler;
			fn->internal_function.handler = hook_handlers[i];
		}
	}
	cairo_hook_sockets();
	cairo_register_pdo_pgsql();
	cairo_fiber_slot = zend_get_resource_handle("cairo");
	if (cairo_fiber_slot >= 0) {
		zend_observer_fiber_switch_register(cairo_fiber_switch);
	}
	cairo_prev_interrupt = zend_interrupt_function;
	zend_interrupt_function = cairo_on_interrupt;
	return SUCCESS;
}

static zend_module_entry cairo_module_entry = {
	STANDARD_MODULE_HEADER,
	"cairo",
	cairo_functions,
	PHP_MINIT(cairo), NULL, NULL, NULL, NULL,
	"0.1.0-dev",
	STANDARD_MODULE_PROPERTIES
};

// --- The cairo SAPI ---------------------------------------------------------
//
// Cairo is its own SAPI (PHP_SAPI is "cairo"), adapted from PHP's embed SAPI
// and FrankenPHP: the boot thread starts PHP and runs the declare phase, and
// each worker thread runs one long PHP request (cairo_thread_run). Requests
// never reach PHP through the SAPI; Go hands them to the runtime script.

// Defaults a php.ini may override (lowest precedence).
static void cairo_ini_defaults(HashTable *configuration_hash)
{
	static const char *defaults[][2] = {
		{"html_errors", "0"},
		{"implicit_flush", "1"},
		{"output_buffering", "0"},
		{"register_argc_argv", "1"},
		{"display_errors", "0"},   // errors go to the log (below), once
		{"log_errors", "1"},
	};
	for (size_t i = 0; i < sizeof(defaults) / sizeof(defaults[0]); i++) {
		zval v;
		ZVAL_NEW_STR(&v, zend_string_init(defaults[i][1], strlen(defaults[i][1]), 1));
		zend_hash_str_update(configuration_hash, defaults[i][0], strlen(defaults[i][0]), &v);
	}
}

// Settings no php.ini may change: each worker thread is one PHP request that
// lasts as long as the server, so PHP's own time limits would end it. Request
// deadlines are Cairo's (Middleware::timeout()).
static const char cairo_forced_ini[] =
	"max_execution_time=0\n"
	"max_input_time=-1\n";

static size_t cairo_ub_write(const char *str, size_t len)
{
	// Output outside an output buffer (isolated mode buffers each request)
	// goes to stdout, as with the CLI.
	size_t done = 0;
	while (done < len) {
		ssize_t n = write(STDOUT_FILENO, str + done, len - done);
		if (n <= 0) {
			if (n < 0 && errno == EINTR) {
				continue;
			}
			break;
		}
		done += (size_t)n;
	}
	return len;
}

static void cairo_flush(void *server_context)
{
}

static void cairo_send_header(sapi_header_struct *sapi_header, void *server_context)
{
}

static char *cairo_read_cookies(void)
{
	return NULL;
}

static void cairo_register_variables(zval *track_vars_array)
{
	php_import_environment_variables(track_vars_array);
}

// PHP's log lines (errors, warnings, error_log()) go to Cairo's logger,
// unless the error_log setting names a file.
static void cairo_log_message(const char *message, int syslog_type)
{
	goPHPLog(cairo_tid, (char *)message, syslog_type);
}

// ---- Opcache ------------------------------------------------------------------
//
// Cairo loads opcache (built as opcache.so next to PHP) unless CAIRO_OPCACHE=0
// or php.ini already loads it. Opcache only starts under the SAPIs it knows,
// and checks the name once, while Zend extensions start; so the SAPI goes by
// "cli-server" for that moment (from the cairo module's MINIT, which runs just
// before) and gets its own name back afterwards. PHP_SAPI and php_sapi_name()
// keep reporting "cairo".

static const char *cairo_sapi_name;
static char *cairo_ini_entries;

static bool cairo_opcache_wanted(void)
{
	const char *env = getenv("CAIRO_OPCACHE");
	if (env != NULL && (strcmp(env, "0") == 0 || strcasecmp(env, "off") == 0 || strcasecmp(env, "false") == 0)) {
		return false;
	}
	return access(PHP_EXTENSION_DIR "/opcache.so", R_OK) == 0;
}

static void cairo_opcache_disguise(void)
{
	if (cairo_sapi_name == NULL && cairo_ini_entries != NULL) {
		cairo_sapi_name = sapi_module.name;
		sapi_module.name = "cli-server";
	}
}

static void cairo_opcache_restore(void)
{
	if (cairo_sapi_name != NULL) {
		sapi_module.name = (char *)cairo_sapi_name;
		cairo_sapi_name = NULL;
	}
}

// Module startup registers the cairo module (and its functions), like any
// PHP extension, with opcache added to the INI entries when wanted.
static int cairo_sapi_startup(sapi_module_struct *sapi)
{
	if (cairo_opcache_wanted()) {
		// Cairo's fixed settings, plus opcache. INI entries are parsed
		// like php.ini lines, so zend_extension= loads it; a php.ini that loads
		// it too only gets a warning.
		const char *base = sapi->ini_entries != NULL ? sapi->ini_entries : "";
		const char *add = "zend_extension=" PHP_EXTENSION_DIR "/opcache.so\n";
		cairo_ini_entries = malloc(strlen(base) + strlen(add) + 1);
		if (cairo_ini_entries != NULL) {
			strcpy(cairo_ini_entries, base);
			strcat(cairo_ini_entries, add);
			sapi->ini_entries = cairo_ini_entries;
		}
	}
	int rc = php_module_startup(sapi, &cairo_module_entry);
	cairo_opcache_restore();
	return rc;
}

static sapi_module_struct cairo_sapi_module = {
	"cairo",                      /* name */
	"Cairo",                      /* pretty name */

	cairo_sapi_startup,           /* startup */
	php_module_shutdown_wrapper,  /* shutdown */

	NULL,                         /* activate */
	NULL,                         /* deactivate */

	cairo_ub_write,               /* unbuffered write */
	cairo_flush,                  /* flush */
	NULL,                         /* get uid */
	NULL,                         /* getenv */

	php_error,                    /* error handler */

	NULL,                         /* header handler */
	NULL,                         /* send headers handler */
	cairo_send_header,            /* send header handler */

	NULL,                         /* read POST data */
	cairo_read_cookies,           /* read Cookies */

	cairo_register_variables,     /* register server variables */
	cairo_log_message,            /* log message */
	NULL,                         /* get request time */
	NULL,                         /* child terminate */

	STANDARD_SAPI_MODULE_PROPERTIES
};

static int run_file(const char *file)
{
	zend_file_handle fh;
	zend_stream_init_filename(&fh, file);
	bool ok = php_execute_script(&fh);
	zend_destroy_file_handle(&fh);
	if (!ok) {
		return -1;
	}
	return EG(exit_status);
}

// cairo_boot starts PHP (module startup, then a request on this thread for
// the declare phase) and runs the runtime script. php_ini is a php.ini path
// (file or directory), or NULL to search the usual places.
int cairo_boot(const char *runtime_file, const char *php_ini)
{
	static char *argv[] = {"cairo", NULL};
	// Sockets whose peer went away must not kill the process.
	signal(SIGPIPE, SIG_IGN);
#ifdef ZTS
	php_tsrm_startup();
#endif
	zend_signal_startup();
	sapi_startup(&cairo_sapi_module);   // clears ini_entries; set them after

	cairo_sapi_module.ini_defaults = cairo_ini_defaults;
	cairo_sapi_module.ini_entries = cairo_forced_ini;
	cairo_sapi_module.php_ini_ignore_cwd = 1;
	if (php_ini != NULL && php_ini[0] != '\0') {
		cairo_sapi_module.php_ini_path_override = strdup(php_ini);
	}
	cairo_sapi_module.executable_location = argv[0];

	if (cairo_sapi_module.startup(&cairo_sapi_module) == FAILURE) {
		return -1;
	}
	SG(options) |= SAPI_OPTION_NO_CHDIR;
	SG(request_info).argc = 1;
	SG(request_info).argv = argv;
	if (php_request_startup() == FAILURE) {
		php_module_shutdown();
		return -1;
	}
	SG(headers_sent) = 1;
	SG(request_info).no_headers = 1;
	php_register_variable("PHP_SELF", "-", NULL);

	cairo_tid = -1;
	return run_file(runtime_file);
}

void cairo_shutdown(void)
{
	php_request_shutdown(NULL);
	php_module_shutdown();
	sapi_shutdown();
#ifdef ZTS
	tsrm_shutdown();
#endif
}

int cairo_thread_run(int tid, const char *runtime_file)
{
	cairo_tid = tid;
	(void)ts_resource(0);
	if (php_request_startup() == FAILURE) {
		ts_free_thread();
		return -1;
	}
	cairo_interrupt_register();
	int status = run_file(runtime_file);
	cairo_interrupt_unregister();
	php_request_shutdown(NULL);
	ts_free_thread();
	return status;
}

// PHP worker threads are created here in C, not by Go. PHP runs each fiber
// on its own C stack, and Go only accepts callbacks from a foreign stack on
// threads it did not create, so cairo_* functions called inside a fiber work.
struct cairo_thread_arg {
	int tid;
	char *file;
};

static void *cairo_thread_main(void *p)
{
	struct cairo_thread_arg *arg = p;
	int status = cairo_thread_run(arg->tid, arg->file);
	int tid = arg->tid;
	free(arg->file);
	free(arg);
	goThreadDone(tid, status);
	return NULL;
}

int cairo_thread_start(int tid, const char *runtime_file)
{
	struct cairo_thread_arg *arg = malloc(sizeof(*arg));
	if (arg == NULL) {
		return -1;
	}
	arg->tid = tid;
	arg->file = strdup(runtime_file);

	pthread_attr_t attr;
	pthread_attr_init(&attr);
	pthread_attr_setstacksize(&attr, 16 * 1024 * 1024);
	pthread_attr_setdetachstate(&attr, PTHREAD_CREATE_DETACHED);
	pthread_t th;
	int rc = pthread_create(&th, &attr, cairo_thread_main, arg);
	pthread_attr_destroy(&attr);
	if (rc != 0) {
		free(arg->file);
		free(arg);
		return -1;
	}
	return 0;
}
