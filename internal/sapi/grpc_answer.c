// The answer to a gRPC call, on its way from PHP to Go.

#include <limits.h>

#include <php.h>
#include <zend_smart_str.h>

#include "extension.h"
#include "grpc_answer.h"
#include "wire.h"

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_respond_grpc, 0, 0, 4)
	ZEND_ARG_INFO(0, id)
	ZEND_ARG_INFO(0, ok)
	ZEND_ARG_INFO(0, code)
	ZEND_ARG_INFO(0, message)
ZEND_END_ARG_INFO()

// cairo_respond_grpc(int $id, bool $ok, int $code, mixed $message): void
// answers a gRPC call: with the reply the handler returned, which goes to
// Go in internal/wire's form (no JSON on either side), or with a status
// code and its text. What Go gets first is $ok in one byte and $code in
// four: internal/grpcserver's answer() reads them.
static PHP_FUNCTION(cairo_respond_grpc)
{
	zend_long id, code;
	bool ok;
	zval *message;
	ZEND_PARSE_PARAMETERS_START(4, 4)
		Z_PARAM_LONG(id)
		Z_PARAM_BOOL(ok)
		Z_PARAM_LONG(code)
		Z_PARAM_ZVAL(message)
	ZEND_PARSE_PARAMETERS_END();
	uint32_t c = (uint32_t)code;
	char meta[5] = {(char)ok, (char)(c & 0xff), (char)(c >> 8 & 0xff), (char)(c >> 16 & 0xff), (char)(c >> 24 & 0xff)};
	smart_str body = {0};
	if (ok) {
		if (cairo_wire_encode_value(message, &body) == FAILURE) {
			smart_str_free(&body);
			RETURN_THROWS();
		}
	} else {
		zend_string *text = zval_try_get_string(message);
		if (text == NULL) {
			RETURN_THROWS();
		}
		smart_str_append(&body, text);
		zend_string_release(text);
	}
	size_t len = body.s != NULL ? ZSTR_LEN(body.s) : 0;
	if (len > INT_MAX) {
		smart_str_free(&body);
		zend_value_error("cairo_respond_grpc(): the answer is larger than 2 GB");
		RETURN_THROWS();
	}
	int queued = cairo_answer(id, meta, sizeof(meta), body.s != NULL ? ZSTR_VAL(body.s) : "", len);
	smart_str_free(&body);
	if (queued != 0) {
		zend_throw_error(NULL, "cairo_respond_grpc(): no memory for the answer");
		RETURN_THROWS();
	}
}

static const zend_function_entry cairo_grpc_answer_functions[] = {
	PHP_FE(cairo_respond_grpc, arginfo_cairo_respond_grpc)
	PHP_FE_END
};

const cairo_extension cairo_grpc_answer = {.functions = cairo_grpc_answer_functions};
