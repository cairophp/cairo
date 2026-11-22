// The replies of a streaming gRPC method, on their way from PHP to Go.

#include <limits.h>
#include <stdlib.h>

#include <php.h>
#include <zend_smart_str.h>

#include "extension.h"
#include "grpc_stream.h"
#include "wire.h"
#include "_cgo_export.h"

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_grpc_send, 0, 0, 2)
	ZEND_ARG_INFO(0, stream)
	ZEND_ARG_INFO(0, message)
ZEND_END_ARG_INFO()

// cairo_grpc_send(int $stream, mixed $message): bool|array queues one reply
// of the streaming method that PHP is serving. A stream has many replies,
// so this is one call into Go with the reply in internal/wire's form and
// nothing to decode on the way back: true when the reply is queued, false
// when the queue is full (the grpc.server.send operation then waits for
// room), or [code, text] when the stream or the reply failed.
static PHP_FUNCTION(cairo_grpc_send)
{
	zend_long stream;
	zval *message;
	ZEND_PARSE_PARAMETERS_START(2, 2)
		Z_PARAM_LONG(stream)
		Z_PARAM_ZVAL(message)
	ZEND_PARSE_PARAMETERS_END();
	smart_str reply = {0};
	if (cairo_wire_encode_value(message, &reply) == FAILURE) {
		smart_str_free(&reply);
		RETURN_THROWS();
	}
	if (ZSTR_LEN(reply.s) > INT_MAX) {
		smart_str_free(&reply);
		zend_value_error("cairo_grpc_send(): the reply is larger than 2 GB");
		RETURN_THROWS();
	}
	char *error = NULL;
	int code = goGrpcSend(stream, ZSTR_VAL(reply.s), (int)ZSTR_LEN(reply.s), &error);
	smart_str_free(&reply);
	if (error == NULL) {
		RETURN_BOOL(code);
	}
	array_init_size(return_value, 2);
	add_next_index_long(return_value, code);
	add_next_index_string(return_value, error);
	free(error);
}

static const zend_function_entry cairo_grpc_stream_functions[] = {
	PHP_FE(cairo_grpc_send, arginfo_cairo_grpc_send)
	PHP_FE_END
};

const cairo_extension cairo_grpc_stream = {.functions = cairo_grpc_stream_functions};
