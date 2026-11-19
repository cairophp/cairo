// Socket hooks: PHP's tcp, unix and TLS stream transports through Go.
//
// Cairo re-registers the tcp and unix stream transports. Streams are still
// created by PHP's own socket code, but get a copy of its ops whose read and
// write first wait for the descriptor through Go (epoll), suspending only the
// calling fiber. Everything built on PHP streams benefits: mysqlnd (PDO MySQL,
// mysqli), Predis and phpredis, fsockopen(), stream_socket_client(). The
// connect itself still blocks for now.

#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <pthread.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <php.h>
#include <php_network.h>
#include <ext/json/php_json.h>
#include <ext/standard/php_fopen_wrappers.h>
#include <zend_smart_str.h>
#include "hooks.h"
#include "_cgo_export.h"

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

const cairo_extension cairo_socket_hooks = {.startup = cairo_hook_sockets};
