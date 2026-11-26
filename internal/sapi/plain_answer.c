// The answer of a script served in C (plain_request.c): what it writes and
// the headers it sets, held until it ends, as Cairo's classic answer is
// (HttpOutput::classic() in php/http_stream.php), or streamed from flush()
// and from 1 MiB on; and fastcgi_finish_request() and headers_send().

#include <time.h>

#include <php.h>
#include <php_output.h>
#include <SAPI.h>
#include <zend_smart_str.h>

#include "extension.h"
#include "http_answer.h"
#include "plain_answer.h"
#include "_cgo_export.h"

// What a page holds before it streams, once its code has closed the buffer
// that stands for the SAPI's (WordPress closes every level at its end): a
// page goes whole, with its length.
#define HOLD (1 << 20)

static __thread int64_t call;      // 0: no script served in C runs
static __thread smart_str out;     // the body that has not gone
static __thread smart_str head;    // the status and headers, as decodeAnswer reads them
static __thread int64_t stream;    // the answer's body goes on this stream (goPlainOpen)
static __thread bool answered;     // fastcgi_finish_request(): what comes now is dropped
static __thread bool failed;       // the client or the call is gone

// Cairo's PHP answers where no script runs in C: callable("Class::method").
static void forward(const char *callable, uint32_t argc, zval *args, zval *return_value)
{
	zval fn;
	ZVAL_STRING(&fn, callable);
	call_user_function(NULL, NULL, &fn, return_value, argc, args);
	zval_ptr_dtor(&fn);
}

void cairo_plain_answer_begin(int64_t id)
{
	call = id;
}

// Sends out on the stream, which is opened with the head first.
static void send_out(void)
{
	if (failed) {
		smart_str_free(&out);
		return;
	}
	if (stream == 0) {
		if (!SG(headers_sent)) {
			sapi_send_headers();
		}
		stream = head.s != NULL ? goPlainOpen(cairo_thread_id(), call, ZSTR_VAL(head.s), (int)ZSTR_LEN(head.s)) : -1;
		failed = stream < 0;
	}
	size_t len = out.s != NULL ? ZSTR_LEN(out.s) : 0;
	for (size_t at = 0; !failed && at < len; at += HOLD) {
		size_t n = len - at < HOLD ? len - at : HOLD;
		failed = goPlainSend(cairo_thread_id(), call, stream, ZSTR_VAL(out.s) + at, (int)n) != 0;
	}
	smart_str_free(&out);
}

size_t cairo_plain_write(const char *s, size_t len)
{
	if (answered) {
		return len;
	}
	smart_str_appendl(&out, s, len);
	if (stream != 0 || ZSTR_LEN(out.s) >= HOLD) {
		send_out();
	}
	return len;
}

// flush(): what the script wrote goes to the client now, its buffer that
// stands for the SAPI's too, unless it opened one of its own.
void cairo_plain_flush(void)
{
	if (answered) {
		return;
	}
	if (php_output_get_level() == 1) {
		php_output_flush();
	}
	send_out();
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

static int encode_head(int status, smart_str *to)
{
	HashTable groups;
	zend_hash_init(&groups, 8, NULL, ZVAL_PTR_DTOR, 0);
	group_headers(&SG(sapi_headers), &groups);
	int rc = cairo_encode_http_answer(status, &groups, to);
	zend_hash_destroy(&groups);
	return rc;
}

int cairo_plain_send_headers(void *headers)
{
	sapi_headers_struct *h = headers;
	smart_str_free(&head);
	return encode_head(h->http_response_code ? h->http_response_code : 200, &head) == SUCCESS
		? SAPI_HEADER_SENT_SUCCESSFULLY : SAPI_HEADER_SEND_FAILED;
}

// Answers the call: status and headers as encoded in meta, and the body.
static void answer_with(const char *meta, size_t n, const char *body, size_t bn)
{
	if (cairo_answer(call, meta, n, body, bn) != 0) {
		static const char none[] = {0x03, (char)0xf4, 0x01, 0, 0, 0, 0, 0, 0, 0x07, 0, 0, 0, 0};   // 500, no headers
		cairo_answer(call, none, sizeof(none), "", 0);
	}
}

void cairo_plain_answer_text(int status, const char *text)
{
	answered = true;
	zval list, headers;
	array_init(&headers);
	array_init(&list);
	add_next_index_string(&list, "text/plain; charset=utf-8");
	zend_hash_str_update(Z_ARRVAL(headers), "Content-Type", sizeof("Content-Type") - 1, &list);
	smart_str meta = {0};
	cairo_encode_http_answer(status, Z_ARRVAL(headers), &meta);
	zval_ptr_dtor(&headers);
	answer_with(ZSTR_VAL(meta.s), ZSTR_LEN(meta.s), text, strlen(text));
	smart_str_free(&meta);
}

// The answer as it stands: the head and what was written, or, once the
// body streams, the end of the stream with the status the script ended
// with (Go cuts a stream that failed).
static void answer_now(int status)
{
	if (stream != 0) {
		send_out();
		HashTable none;
		zend_hash_init(&none, 0, NULL, NULL, 0);
		smart_str meta = {0};
		cairo_encode_http_answer(status, &none, &meta);
		answer_with(ZSTR_VAL(meta.s), ZSTR_LEN(meta.s), "", 0);
		smart_str_free(&meta);
		return;
	}
	if (!SG(headers_sent)) {
		sapi_send_headers();   // the script wrote nothing
	}
	if (head.s == NULL || (out.s != NULL && ZSTR_LEN(out.s) > INT_MAX)) {
		cairo_plain_answer_text(500, "Internal Server Error\n");
		return;
	}
	answer_with(ZSTR_VAL(head.s), ZSTR_LEN(head.s), out.s ? ZSTR_VAL(out.s) : "", out.s ? ZSTR_LEN(out.s) : 0);
	smart_str_free(&out);
}

static bool past_deadline(void)
{
	int64_t at = cairo_stack_deadline();
	struct timespec now;
	clock_gettime(CLOCK_MONOTONIC, &now);
	return at != 0 && (int64_t)now.tv_sec * 1000000000 + now.tv_nsec >= at;
}

// Whether a fatal error ended the script (an uncaught exception too).
static bool fatal(void)
{
	return EG(exit_status) == 255 && PG(last_error_message) != NULL
		&& (PG(last_error_type) & (E_ERROR | E_CORE_ERROR | E_COMPILE_ERROR | E_USER_ERROR | E_PARSE | E_RECOVERABLE_ERROR));
}

void cairo_plain_answer(void)
{
	if (answered) {
		return;
	}
	answered = true;
	if (past_deadline()) {
		if (stream == 0) {
			cairo_plain_answer_text(504, "Gateway Timeout\n");   // as Go's own timeout answers
		} else {
			answer_now(504);
		}
		return;
	}
	// A fatal error is PHP's own: a 500 with what was written, as under
	// PHP-FPM, which PHP set unless the head had gone.
	answer_now(fatal() ? 500 : 200);
}

void cairo_plain_answer_free(void)
{
	smart_str_free(&out);
	smart_str_free(&head);
}

void cairo_plain_answer_end(void)
{
	call = stream = 0;
	answered = failed = false;
	out = (smart_str){0};
	head = (smart_str){0};
}

// fastcgi_finish_request(): bool
// The client gets the whole answer now, and the script runs on, past the
// request's deadline too, as under PHP-FPM. Elsewhere Cairo's PHP answers
// (HttpOutput::finish()).
static PHP_FUNCTION(fastcgi_finish_request)
{
	ZEND_PARSE_PARAMETERS_NONE();
	if (call == 0) {
		forward("Cairo\\Internal\\HttpOutput::finish", 0, NULL, return_value);
		return;
	}
	if (answered) {
		RETURN_FALSE;
	}
	php_output_end_all();
	cairo_plain_answer();
	cairo_stack_set_deadline(0);
	RETURN_TRUE;
}

// headers_send(int $status = 200): int
// With 1xx, the headers set so far go as Early Hints, and stay for the
// answer; else the head of the answer goes now.
static PHP_FUNCTION(headers_send)
{
	zend_long status = 200;
	ZEND_PARSE_PARAMETERS_START(0, 1)
		Z_PARAM_OPTIONAL
		Z_PARAM_LONG(status)
	ZEND_PARSE_PARAMETERS_END();
	if (call == 0) {
		zval arg;
		ZVAL_LONG(&arg, status);
		forward("Cairo\\Internal\\HttpOutput::headersSend", 1, &arg, return_value);
		return;
	}
	if (status >= 100 && status < 200) {
		smart_str hint = {0};
		if (!answered && stream == 0 && encode_head((int)status, &hint) == SUCCESS) {
			goPlainHint(cairo_thread_id(), call, ZSTR_VAL(hint.s), (int)ZSTR_LEN(hint.s));
		}
		smart_str_free(&hint);
		RETURN_LONG(status);
	}
	if (!SG(headers_sent)) {
		SG(sapi_headers).http_response_code = (int)status;
	}
	cairo_plain_flush();
	RETURN_LONG(status);
}

ZEND_BEGIN_ARG_WITH_RETURN_TYPE_INFO_EX(arginfo_fastcgi_finish_request, 0, 0, _IS_BOOL, 0)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_WITH_RETURN_TYPE_INFO_EX(arginfo_headers_send, 0, 0, IS_LONG, 0)
	ZEND_ARG_TYPE_INFO_WITH_DEFAULT_VALUE(0, status, IS_LONG, 0, "200")
ZEND_END_ARG_INFO()

static const zend_function_entry functions[] = {
	PHP_FE(fastcgi_finish_request, arginfo_fastcgi_finish_request)
	PHP_FE(headers_send, arginfo_headers_send)
	PHP_FE_END
};

// What classic PHP code calls where a server runs it: not on the command
// line (cairo php), whose SAPI shares the module and has neither, as PHP's.
void cairo_plain_answer_startup(void)
{
	if (strcmp(sapi_module.name, "cli") != 0) {
		zend_register_functions(NULL, functions, NULL, MODULE_PERSISTENT);
	}
}
