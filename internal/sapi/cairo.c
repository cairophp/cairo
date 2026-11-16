// Cairo's bridge between Go and an embedded, thread-safe (ZTS) PHP.
//
// Milestone 1 builds on PHP's own embed SAPI: one boot thread runs the
// declare phase, then each PHP worker thread gets its own interpreter context
// (ts_resource) and runs the Cairo runtime script, which loops on
// cairo_next_event() and runs every request in its own Fiber.

#include <pthread.h>
#include <stdlib.h>
#include <string.h>

#include <php.h>
#include <php_main.h>
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
#include <poll.h>
#include <sapi/embed/php_embed.h>
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

static const zend_function_entry cairo_functions[] = {
	PHP_FE(cairo_info, arginfo_cairo_none)
	PHP_FE(cairo_spec, arginfo_cairo_spec)
	PHP_FE(cairo_next_event, arginfo_cairo_none)
	PHP_FE(cairo_respond, arginfo_cairo_respond)
	PHP_FE(cairo_fiber_open, arginfo_cairo_fiber_open)
	PHP_FE(cairo_fiber_close, arginfo_cairo_fiber_close)
	PHP_FE(cairo_fiber_op, arginfo_cairo_fiber_op)
	PHP_FE(cairo_go_call, arginfo_cairo_go_call)
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

static int cairo_sock_set_option(php_stream *stream, int option, int value, void *ptrparam)
{
	const php_stream_ops *orig = find_orig_ops(stream->ops);
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
			st = malloc(sizeof(*st));
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

// Module startup: install the hooks once, before any worker thread copies
// the function table.
static PHP_MINIT_FUNCTION(cairo)
{
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

// Replaces the embed SAPI's startup hook so the cairo module (and its
// functions) is registered during module startup, like any PHP extension.
static int cairo_sapi_startup(sapi_module_struct *sapi)
{
	return php_module_startup(sapi, &cairo_module_entry);
}

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

int cairo_boot(const char *runtime_file)
{
	static char *argv[] = {"cairo", NULL};
	php_embed_module.startup = cairo_sapi_startup;
	if (php_embed_init(1, argv) == FAILURE) {
		return -1;
	}
	cairo_tid = -1;
	return run_file(runtime_file);
}

void cairo_shutdown(void)
{
	php_embed_shutdown();
}

int cairo_thread_run(int tid, const char *runtime_file)
{
	cairo_tid = tid;
	(void)ts_resource(0);
	if (php_request_startup() == FAILURE) {
		ts_free_thread();
		return -1;
	}
	int status = run_file(runtime_file);
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
