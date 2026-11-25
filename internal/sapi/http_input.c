// php://input for the script of a classic request. Cairo's SAPI reads no
// body: Go hands it to PHP in the request's event, and this gives it to the
// SAPI where php://input looks.

#include <php.h>
#include <SAPI.h>

#include "http_input.h"

// cairo_http_input(string $body): void
// The body that php://input reads in this PHP request, which has not read
// one yet: classic mode has a PHP request for each request.
PHP_FUNCTION(cairo_http_input)
{
	zend_string *body;
	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_STR(body)
	ZEND_PARSE_PARAMETERS_END();
	// A stream that php://input opened has this one: it is not replaced.
	if (SG(request_info).request_body != NULL) {
		zend_throw_error(NULL, "cairo_http_input(): this PHP request has its body already");
		RETURN_THROWS();
	}
	php_stream *s = php_stream_temp_create_ex(TEMP_STREAM_DEFAULT, SAPI_POST_BLOCK_SIZE, PG(upload_tmp_dir));
	if (s == NULL) {
		zend_throw_error(NULL, "cairo_http_input(): cannot hold the body");
		RETURN_THROWS();
	}
	if (php_stream_write(s, ZSTR_VAL(body), ZSTR_LEN(body)) != (ssize_t)ZSTR_LEN(body)) {
		php_stream_close(s);
		zend_throw_error(NULL, "cairo_http_input(): cannot hold the body");
		RETURN_THROWS();
	}
	SG(request_info).request_body = s;
}

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_http_input, 0, 0, 1)
	ZEND_ARG_INFO(0, body)
ZEND_END_ARG_INFO()

static const zend_function_entry cairo_http_input_functions[] = {
	PHP_FE(cairo_http_input, arginfo_cairo_http_input)
	PHP_FE_END
};

const cairo_extension cairo_http_input = {.functions = cairo_http_input_functions};
