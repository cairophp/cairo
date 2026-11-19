// The answer to an HTTP request, on its way from PHP to Go.

#include <limits.h>

#include <php.h>
#include <zend_smart_str.h>

#include "http_answer.h"
#include "wire.h"
#include "_cgo_export.h"

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_respond_http, 0, 0, 4)
	ZEND_ARG_INFO(0, id)
	ZEND_ARG_INFO(0, status)
	ZEND_ARG_INFO(0, headers)
	ZEND_ARG_INFO(0, body)
ZEND_END_ARG_INFO()

// cairo_respond_http(int $id, int $status, array $headers, string $body):
// void answers an HTTP request. The status and headers go to Go in
// internal/wire's format (no JSON on either side), the body as raw bytes.
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
	if (cairo_wire_encode_response(status, headers, &meta) == FAILURE || ZSTR_LEN(meta.s) > INT_MAX) {
		smart_str_free(&meta);
		if (EG(exception) == NULL) {
			zend_value_error("cairo_respond_http(): the headers are larger than 2 GB");
		}
		RETURN_THROWS();
	}
	goRespond((long long)id, ZSTR_VAL(meta.s), (int)ZSTR_LEN(meta.s), body, (int)blen);
	smart_str_free(&meta);
}

static const zend_function_entry cairo_http_answer_functions[] = {
	PHP_FE(cairo_respond_http, arginfo_cairo_respond_http)
	PHP_FE_END
};

const cairo_extension cairo_http_answer = {.functions = cairo_http_answer_functions};
