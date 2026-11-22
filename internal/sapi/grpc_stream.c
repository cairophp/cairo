// What PHP sends on a gRPC stream, on its way to Go: the replies of a
// streaming method it serves, and the requests of a stream it opened as a
// client.

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

// A stream has many messages, so sending one is one call into Go with the
// message in internal/wire's form and nothing to decode on the way back:
// true when it is queued, false when the queue is full (a Go operation then
// waits for room), or [code, text] when the stream or the message failed.
static void grpc_send(INTERNAL_FUNCTION_PARAMETERS, int (*send)(long long, char *, int, char **))
{
	zend_long stream;
	zval *message;
	ZEND_PARSE_PARAMETERS_START(2, 2)
		Z_PARAM_LONG(stream)
		Z_PARAM_ZVAL(message)
	ZEND_PARSE_PARAMETERS_END();
	smart_str encoded = {0};
	if (cairo_wire_encode_value(message, &encoded) == FAILURE) {
		smart_str_free(&encoded);
		RETURN_THROWS();
	}
	if (ZSTR_LEN(encoded.s) > INT_MAX) {
		smart_str_free(&encoded);
		zend_value_error("%s(): the message is larger than 2 GB", get_active_function_name());
		RETURN_THROWS();
	}
	char *error = NULL;
	int code = send(stream, ZSTR_VAL(encoded.s), (int)ZSTR_LEN(encoded.s), &error);
	smart_str_free(&encoded);
	if (error == NULL) {
		RETURN_BOOL(code);
	}
	array_init_size(return_value, 2);
	add_next_index_long(return_value, code);
	add_next_index_string(return_value, error);
	free(error);
}

// cairo_grpc_send(int $stream, mixed $message): bool|array queues one reply
// of the streaming method that PHP is serving (grpc.server.send waits for
// room).
static PHP_FUNCTION(cairo_grpc_send)
{
	grpc_send(INTERNAL_FUNCTION_PARAM_PASSTHRU, goGrpcSend);
}

// cairo_grpc_client_send(int $stream, mixed $message): bool|array queues
// one request of a stream that PHP opened as a client (grpc.stream.send
// waits for room).
static PHP_FUNCTION(cairo_grpc_client_send)
{
	grpc_send(INTERNAL_FUNCTION_PARAM_PASSTHRU, goGrpcClientSend);
}

static const zend_function_entry cairo_grpc_stream_functions[] = {
	PHP_FE(cairo_grpc_send, arginfo_cairo_grpc_send)
	PHP_FE(cairo_grpc_client_send, arginfo_cairo_grpc_send)
	PHP_FE_END
};

const cairo_extension cairo_grpc_stream = {.functions = cairo_grpc_stream_functions};
