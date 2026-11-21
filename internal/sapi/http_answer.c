// The answer to an HTTP request, on its way from PHP to Go.

#include <limits.h>

#include <php.h>
#include <zend_smart_str.h>

#include "extension.h"
#include "http_answer.h"
#include "wire.h"
#include "_cgo_export.h"

static void put_u32(smart_str *out, uint32_t v)
{
	char b[4] = {(char)(v & 0xff), (char)(v >> 8 & 0xff), (char)(v >> 16 & 0xff), (char)(v >> 24 & 0xff)};
	smart_str_appendl(out, b, 4);
}

static void put_string(smart_str *out, const char *s, size_t len)
{
	smart_str_appendc(out, TAG_STRING);
	put_u32(out, (uint32_t)len);
	smart_str_appendl(out, s, len);
}

// Appends the status (an int) and the headers (a map of lists of strings; a
// value that is not an array is a list of one) with internal/wire's tags:
// what internal/httpserver's decodeAnswer reads. Returns FAILURE when a
// header value cannot be turned into a string (an exception is then pending).
static int encode_answer(zend_long status, HashTable *headers, smart_str *out)
{
	uint64_t u = (uint64_t)(int64_t)status;
	smart_str_appendc(out, TAG_INT);
	for (int i = 0; i < 8; i++) {
		smart_str_appendc(out, (char)(u >> (8 * i) & 0xff));
	}
	smart_str_appendc(out, TAG_MAP);
	put_u32(out, zend_hash_num_elements(headers));
	zend_string *key;
	zend_ulong idx;
	zval *val;
	ZEND_HASH_FOREACH_KEY_VAL(headers, idx, key, val) {
		if (key != NULL) {
			put_u32(out, (uint32_t)ZSTR_LEN(key));
			smart_str_appendl(out, ZSTR_VAL(key), ZSTR_LEN(key));
		} else {
			char num[MAX_LENGTH_OF_LONG + 1];
			int n = snprintf(num, sizeof(num), ZEND_ULONG_FMT, idx);
			put_u32(out, (uint32_t)n);
			smart_str_appendl(out, num, (size_t)n);
		}
		ZVAL_DEREF(val);
		if (Z_TYPE_P(val) != IS_ARRAY) {
			// A single value: header('X: y') style.
			smart_str_appendc(out, TAG_LIST);
			put_u32(out, 1);
			zend_string *s = zval_try_get_string(val);
			if (s == NULL) {
				return FAILURE;
			}
			put_string(out, ZSTR_VAL(s), ZSTR_LEN(s));
			zend_string_release(s);
			continue;
		}
		smart_str_appendc(out, TAG_LIST);
		put_u32(out, zend_hash_num_elements(Z_ARRVAL_P(val)));
		zval *item;
		ZEND_HASH_FOREACH_VAL(Z_ARRVAL_P(val), item) {
			zend_string *s = zval_try_get_string(item);
			if (s == NULL) {
				return FAILURE;
			}
			put_string(out, ZSTR_VAL(s), ZSTR_LEN(s));
			zend_string_release(s);
		} ZEND_HASH_FOREACH_END();
	} ZEND_HASH_FOREACH_END();
	return SUCCESS;
}

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_respond_http, 0, 0, 4)
	ZEND_ARG_INFO(0, id)
	ZEND_ARG_INFO(0, status)
	ZEND_ARG_INFO(0, headers)
	ZEND_ARG_INFO(0, body)
ZEND_END_ARG_INFO()

// cairo_respond_http(int $id, int $status, array $headers, string $body):
// void answers an HTTP request. The status and headers go to Go as
// encode_answer() writes them (no JSON on either side), the body as raw bytes.
static PHP_FUNCTION(cairo_respond_http)
{
	zend_long id, status;
	HashTable *headers;
	char *body;
	size_t blen;
	ZEND_PARSE_PARAMETERS_START(4, 4)
		Z_PARAM_LONG(id)
		Z_PARAM_LONG(status)
		Z_PARAM_ARRAY_HT(headers)
		Z_PARAM_STRING(body, blen)
	ZEND_PARSE_PARAMETERS_END();
	if (blen > INT_MAX) {
		zend_value_error("cairo_respond_http(): the body is larger than 2 GB");
		RETURN_THROWS();
	}
	smart_str meta = {0};
	if (encode_answer(status, headers, &meta) == FAILURE || ZSTR_LEN(meta.s) > INT_MAX) {
		smart_str_free(&meta);
		if (EG(exception) == NULL) {
			zend_value_error("cairo_respond_http(): the headers are larger than 2 GB");
		}
		RETURN_THROWS();
	}
	int queued = cairo_answer(id, ZSTR_VAL(meta.s), ZSTR_LEN(meta.s), body, blen);
	smart_str_free(&meta);
	if (queued != 0) {
		zend_throw_error(NULL, "cairo_respond_http(): no memory for the answer");
		RETURN_THROWS();
	}
}

static const zend_function_entry cairo_http_answer_functions[] = {
	PHP_FE(cairo_respond_http, arginfo_cairo_respond_http)
	PHP_FE_END
};

const cairo_extension cairo_http_answer = {.functions = cairo_http_answer_functions};
