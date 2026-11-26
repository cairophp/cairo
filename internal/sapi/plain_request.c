// The scripts of cairo serve on a folder or a plain script (php/plain.php)
// as PHP's own SAPI requests, the way PHP-FPM serves them: the thread takes
// the call in C (cserve.h in internal/core), PHP's startup fills $_GET,
// $_POST, $_COOKIE and $_FILES from SG(request_info) and these callbacks,
// $_SERVER is registered here, and what the script writes and the headers
// it sets become the answer. No PHP of Cairo's runs around the script.

#include <time.h>

#include <php.h>
#include <php_main.h>
#include <php_output.h>
#include <php_variables.h>
#include <SAPI.h>
#include <zend_exceptions.h>
#include <zend_smart_str.h>
#include <ext/standard/basic_functions.h>

#include "extension.h"
#include "http_answer.h"
#include "plain_request.h"
#include "wire.h"

static __thread bool serving;    // a request of this server runs on the thread
static __thread int64_t call;
static __thread zval ev;         // the call's event (internal/wire, decoded)
static __thread HashTable *script;
static __thread const char *cookie;
static __thread size_t posted;   // what read_post gave of the body
static __thread smart_str out;   // the body
static __thread smart_str head;  // the status and headers, as decodeAnswer reads them

static zval *field(HashTable *ht, const char *key)
{
	return ht != NULL ? zend_hash_str_find(ht, key, strlen(key)) : NULL;
}

// The string under key, NUL-terminated, or "" (len 0).
static const char *str(HashTable *ht, const char *key, size_t *len)
{
	zval *v = field(ht, key);
	if (v == NULL || Z_TYPE_P(v) != IS_STRING) {
		*len = 0;
		return "";
	}
	*len = Z_STRLEN_P(v);
	return Z_STRVAL_P(v);
}

// The first value of request header name (Go has it canonical), or NULL.
static const char *header(const char *name)
{
	zval *hs = field(Z_ARRVAL(ev), "headers");
	zval *list = hs != NULL && Z_TYPE_P(hs) == IS_ARRAY ? field(Z_ARRVAL_P(hs), name) : NULL;
	zval *first = list != NULL && Z_TYPE_P(list) == IS_ARRAY ? zend_hash_index_find(Z_ARRVAL_P(list), 0) : NULL;
	return first != NULL && Z_TYPE_P(first) == IS_STRING ? Z_STRVAL_P(first) : NULL;
}

// The request info that PHP's startup reads, as PHP-FPM sets it.
static void request_info(void)
{
	HashTable *e = Z_ARRVAL(ev);
	size_t n;
	SG(request_info).request_method = str(e, "method", &n);
	SG(request_info).query_string = (char *)str(e, "query", &n);
	SG(request_info).request_uri = (char *)str(e, "path", &n);
	SG(request_info).content_type = header("Content-Type");
	const char *length = header("Content-Length");
	SG(request_info).content_length = length != NULL ? ZEND_STRTOL(length, NULL, 10) : 0;
	cookie = header("Cookie");
	php_handle_auth_data(header("Authorization"));
	SG(sapi_headers).http_response_code = 200;   // PHP's startup leaves the last request's
}

// The deadline of the call, a time of Go's clock (Unix seconds), on the
// clock that the hooks and the hard stop read.
static void deadline(void)
{
	zval *at = field(Z_ARRVAL(ev), "deadline");
	if (at == NULL || Z_TYPE_P(at) != IS_DOUBLE) {
		return;
	}
	struct timespec real, mono;
	clock_gettime(CLOCK_REALTIME, &real);
	clock_gettime(CLOCK_MONOTONIC, &mono);
	double left = Z_DVAL_P(at) - ((double)real.tv_sec + (double)real.tv_nsec / 1e9);
	int64_t now = (int64_t)mono.tv_sec * 1000000000 + mono.tv_nsec;
	int64_t tick = now + (left > 0 ? (int64_t)(left * 1e9) : 0);
	cairo_stack_set_deadline(tick > 0 ? tick : 1);
}

static void begin(int64_t id, const char *event, size_t len)
{
	serving = true;
	call = id;
	if (cairo_wire_decode(event, len, &ev) == FAILURE || Z_TYPE(ev) != IS_ARRAY) {
		ZVAL_UNDEF(&ev);
		return;
	}
	zval *sc = field(Z_ARRVAL(ev), "script");
	script = sc != NULL && Z_TYPE_P(sc) == IS_ARRAY ? Z_ARRVAL_P(sc) : NULL;
	if (script == NULL) {
		zval_ptr_dtor(&ev);
		ZVAL_UNDEF(&ev);
		return;
	}
	request_info();
	deadline();
}

static void put(zval *arr, const char *key, const char *val, size_t len)
{
	php_register_variable_safe(key, val, len, arr);
}

// REMOTE_ADDR and REMOTE_PORT from Go's host:port ([v6]:port).
static void put_remote(zval *arr)
{
	size_t n;
	const char *remote = str(Z_ARRVAL(ev), "remote", &n);
	const char *colon = zend_memrchr(remote, ':', n);
	if (colon == NULL) {
		put(arr, "REMOTE_ADDR", remote, n);
		put(arr, "REMOTE_PORT", "", 0);
		return;
	}
	size_t hl = (size_t)(colon - remote);
	if (hl >= 2 && remote[0] == '[' && remote[hl - 1] == ']') {
		put(arr, "REMOTE_ADDR", remote + 1, hl - 2);
	} else {
		put(arr, "REMOTE_ADDR", remote, hl);
	}
	put(arr, "REMOTE_PORT", colon + 1, n - hl - 1);
}

// One request header as CGI names it: HTTP_X_Y, or CONTENT_TYPE and
// CONTENT_LENGTH as they are, with its values joined by ", ".
static void put_header(zval *arr, zend_string *name, HashTable *values)
{
	char key[256];
	bool plain = zend_string_equals_literal_ci(name, "Content-Type") || zend_string_equals_literal_ci(name, "Content-Length");
	size_t off = plain ? 0 : 5;
	if (ZSTR_LEN(name) + off >= sizeof(key)) {
		return;   // Go's header limit lets no such name through
	}
	memcpy(key, "HTTP_", off);
	for (size_t i = 0; i < ZSTR_LEN(name); i++) {
		char c = ZSTR_VAL(name)[i];
		key[off + i] = c == '-' ? '_' : (char)toupper((unsigned char)c);
	}
	key[off + ZSTR_LEN(name)] = '\0';
	smart_str val = {0};
	zval *item;
	ZEND_HASH_FOREACH_VAL(values, item) {
		if (Z_TYPE_P(item) == IS_STRING) {
			if (val.s != NULL) {
				smart_str_appendl(&val, ", ", 2);
			}
			smart_str_appendl(&val, Z_STRVAL_P(item), Z_STRLEN_P(item));
		}
	} ZEND_HASH_FOREACH_END();
	put(arr, key, val.s ? ZSTR_VAL(val.s) : "", val.s ? ZSTR_LEN(val.s) : 0);
	smart_str_free(&val);
}

static void put_headers(zval *arr)
{
	zval *hs = field(Z_ARRVAL(ev), "headers");
	if (hs == NULL || Z_TYPE_P(hs) != IS_ARRAY) {
		return;
	}
	zend_string *name;
	zval *values;
	ZEND_HASH_FOREACH_STR_KEY_VAL(Z_ARRVAL_P(hs), name, values) {
		if (name != NULL && Z_TYPE_P(values) == IS_ARRAY) {
			put_header(arr, name, Z_ARRVAL_P(values));
		}
	} ZEND_HASH_FOREACH_END();
}

// key is a, then sep and b when there is a b.
static void put_joined(zval *arr, const char *key, const char *a, size_t an, const char *sep, const char *b, size_t bn)
{
	smart_str s = {0};
	smart_str_appendl(&s, a, an);
	if (bn > 0) {
		smart_str_appends(&s, sep);
		smart_str_appendl(&s, b, bn);
	}
	smart_str_0(&s);
	put(arr, key, s.s ? ZSTR_VAL(s.s) : "", s.s ? ZSTR_LEN(s.s) : 0);
	smart_str_free(&s);
}

// $_SERVER with the keys a classic script had before (Superglobals::build()
// and Scripts::runScript()), in their order; PHP adds REQUEST_TIME and
// REQUEST_TIME_FLOAT, and PHP_AUTH_USER and PHP_AUTH_PW as PHP-FPM does.
static void server_vars(zval *arr)
{
	HashTable *e = Z_ARRVAL(ev);
	size_t n, pn, qn, nn, pin;
	const char *path = str(e, "path", &pn), *query = str(e, "query", &qn);
	const char *name = str(script, "name", &nn), *info = str(script, "path_info", &pin);
	const char *v = str(e, "method", &n);
	put(arr, "REQUEST_METHOD", v, n);
	put_joined(arr, "REQUEST_URI", path, pn, "?", query, qn);
	put(arr, "SCRIPT_NAME", name, nn);
	if (pin > 0) {
		put(arr, "PATH_INFO", info, pin);
	}
	put(arr, "QUERY_STRING", query, qn);
	v = str(e, "proto", &n);
	put(arr, "SERVER_PROTOCOL", v, n);
	zval *tls = field(e, "tls");
	bool https = tls != NULL && Z_TYPE_P(tls) == IS_TRUE;
	if (https) {
		put(arr, "HTTPS", "on", 2);   // nginx sends none for a plain request, and PHP-FPM has none
	}
	put_remote(arr);
	v = str(e, "host", &n);
	const char *colon = memchr(v, ':', n);
	put(arr, "SERVER_NAME", v, colon != NULL ? (size_t)(colon - v) : n);
	if (n > 0) {
		put(arr, "HTTP_HOST", v, n);   // Go keeps the Host header apart from the others
	}
	put_headers(arr);
	v = str(script, "file", &n);
	put(arr, "SCRIPT_FILENAME", v, n);
	put_joined(arr, "PHP_SELF", name, nn, "", info, pin);
	v = str(script, "root", &n);
	put(arr, "DOCUMENT_ROOT", v, n);
	v = str(script, "port", &n);
	put(arr, "SERVER_PORT", v, n);
	put(arr, "SERVER_SOFTWARE", "Cairo", 5);
	put(arr, "GATEWAY_INTERFACE", "CGI/1.1", 7);
	put(arr, "REQUEST_SCHEME", https ? "https" : "http", https ? 5 : 4);
}

static char *read_cookies(void)
{
	return (char *)cookie;
}

static size_t read_post(char *buf, size_t len)
{
	size_t n;
	const char *body = Z_TYPE(ev) == IS_ARRAY ? str(Z_ARRVAL(ev), "body", &n) : "";
	if (Z_TYPE(ev) != IS_ARRAY || posted >= n) {
		return 0;
	}
	size_t take = n - posted < len ? n - posted : len;
	memcpy(buf, body + posted, take);
	posted += take;
	return take;
}

static size_t write_out(const char *s, size_t len)
{
	smart_str_appendl(&out, s, len);
	return len;
}

static void flush_out(void)
{
}

// The headers PHP holds for the answer, grouped by name.
static void group_headers(sapi_headers_struct *h, HashTable *groups)
{
	zend_llist_position pos;
	for (sapi_header_struct *hd = zend_llist_get_first_ex(&h->headers, &pos); hd != NULL;
		hd = zend_llist_get_next_ex(&h->headers, &pos)) {
		const char *colon = memchr(hd->header, ':', hd->header_len);
		if (colon == NULL) {
			continue;
		}
		size_t nl = (size_t)(colon - hd->header);
		const char *v = colon + 1;
		while (*v == ' ' || *v == '\t') {
			v++;
		}
		zval *list = zend_hash_str_find(groups, hd->header, nl);
		if (list == NULL) {
			zval l;
			array_init(&l);
			list = zend_hash_str_update(groups, hd->header, nl, &l);
		}
		add_next_index_stringl(list, v, hd->header_len - (size_t)(v - hd->header));
	}
}

static int send_headers(void *headers)
{
	sapi_headers_struct *h = headers;
	HashTable groups;
	zend_hash_init(&groups, 8, NULL, ZVAL_PTR_DTOR, 0);
	group_headers(h, &groups);
	smart_str_free(&head);
	int rc = cairo_encode_http_answer(h->http_response_code ? h->http_response_code : 200, &groups, &head);
	zend_hash_destroy(&groups);
	return rc == SUCCESS ? SAPI_HEADER_SENT_SUCCESSFULLY : SAPI_HEADER_SEND_FAILED;
}

// Answers with status and a short text of Cairo's own, in place of what
// the script wrote. A fatal error is PHP's own: a 500 with what was written.
static void answer_text(int status, const char *text)
{
	zval list, headers;
	array_init(&headers);
	array_init(&list);
	add_next_index_string(&list, "text/plain; charset=utf-8");
	zend_hash_str_update(Z_ARRVAL(headers), "Content-Type", sizeof("Content-Type") - 1, &list);
	smart_str meta = {0};
	cairo_encode_http_answer(status, Z_ARRVAL(headers), &meta);
	zval_ptr_dtor(&headers);
	cairo_answer(call, ZSTR_VAL(meta.s), ZSTR_LEN(meta.s), text, strlen(text));
	smart_str_free(&meta);
}

static bool past_deadline(void)
{
	int64_t at = cairo_stack_deadline();
	struct timespec now;
	clock_gettime(CLOCK_MONOTONIC, &now);
	return at != 0 && (int64_t)now.tv_sec * 1000000000 + now.tv_nsec >= at;
}

static void answer(void)
{
	if (past_deadline()) {
		answer_text(504, "Gateway Timeout\n");   // as Go's own timeout answers
		return;
	}
	sapi_send_headers();   // when the script wrote nothing
	if (head.s == NULL || out.s != NULL && ZSTR_LEN(out.s) > INT_MAX
		|| cairo_answer(call, ZSTR_VAL(head.s), ZSTR_LEN(head.s), out.s ? ZSTR_VAL(out.s) : "", out.s ? ZSTR_LEN(out.s) : 0) != 0) {
		answer_text(500, "Internal Server Error\n");
	}
}

static void clear_exit(void)
{
	if (EG(exception) != NULL && zend_is_unwind_exit(EG(exception))) {
		zend_clear_exception();
	}
}

// Runs the script as PHP-FPM runs a request's, up to what its shutdown does
// before the output goes (shutdown functions, destructors), and answers.
static void serve(void)
{
	if (Z_TYPE(ev) != IS_ARRAY) {
		answer_text(500, "Internal Server Error\n");
		return;
	}
	size_t n;
	const char *file = str(script, "file", &n);
	// As Cairo's classic answer has it: no header that PHP added at start
	// (X-Powered-By), and the script's output in a buffer, so that header()
	// works after echo.
	sapi_header_op(SAPI_HEADER_DELETE_ALL, NULL);
	php_output_start_default();
	char dir[MAXPATHLEN];
	if (n < sizeof(dir)) {
		memcpy(dir, file, n + 1);
		dir[zend_dirname(dir, n)] = '\0';
		VCWD_CHDIR(dir);   // relative includes start where the script is
	}
	zend_file_handle fh;
	zend_stream_init_filename(&fh, file);
	fh.primary_script = 1;
	php_execute_script(&fh);
	zend_destroy_file_handle(&fh);
	clear_exit();
	php_call_shutdown_functions();
	php_free_shutdown_functions();
	clear_exit();
	zend_call_destructors();
	clear_exit();
	php_output_end_all();
	answer();
}

// As the request shuts down, while its memory is still there: what PHP
// reads of the body from now on is none (sapi_deactivate).
static void request_shutdown(void)
{
	if (!serving) {
		return;
	}
	SG(post_read) = 1;
	smart_str_free(&out);
	smart_str_free(&head);
	if (Z_TYPE(ev) != IS_UNDEF) {
		zval_ptr_dtor(&ev);
		ZVAL_UNDEF(&ev);
	}
}

// The PHP request has ended: nothing here points into its memory any more.
static void end(void)
{
	serving = false;
	call = 0;
	script = NULL;
	cookie = NULL;
	posted = 0;
	ZVAL_UNDEF(&ev);
	out = (smart_str){0};
	head = (smart_str){0};
	SG(request_info).request_method = NULL;
	SG(request_info).query_string = NULL;
	SG(request_info).request_uri = NULL;
	SG(request_info).content_type = NULL;
	SG(request_info).content_length = 0;
	SG(request_info).cookie_data = NULL;
}

static const cairo_cserver server = {
	.begin = begin,
	.serve = serve,
	.end = end,
	.read_cookies = read_cookies,
	.register_variables = server_vars,
	.read_post = read_post,
	.write = write_out,
	.flush = flush_out,
	.send_headers = send_headers,
};

const cairo_extension cairo_plain_request = {
	.request_shutdown = request_shutdown,
	.cserver = &server,
};
