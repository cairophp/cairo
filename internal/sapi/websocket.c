// A WebSocket's messages between PHP and Go. Neither call waits: a message
// that PHP sends is queued in Go for the goroutine that writes, and one
// that the client sent was read there ahead of PHP. Cairo\Http\WebSocket
// waits in a Go operation only when the queue is full or nothing has
// arrived.

#include <limits.h>
#include <stdlib.h>

#include <php.h>

#include "extension.h"
#include "websocket.h"
#include "_cgo_export.h"

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_ws_send, 0, 0, 3)
	ZEND_ARG_INFO(0, socket)
	ZEND_ARG_INFO(0, data)
	ZEND_ARG_INFO(0, binary)
ZEND_END_ARG_INFO()

// cairo_ws_send(int $socket, string $data, bool $binary): bool|int queues a
// message for the client: true when it is queued, false when the queue is
// full (the Go operation http.ws.send then waits for room), -1 when the
// client is gone and -2 when the socket has ended.
static PHP_FUNCTION(cairo_ws_send)
{
	zend_long socket;
	char *data;
	size_t len;
	bool binary;
	ZEND_PARSE_PARAMETERS_START(3, 3)
		Z_PARAM_LONG(socket)
		Z_PARAM_STRING(data, len)
		Z_PARAM_BOOL(binary)
	ZEND_PARSE_PARAMETERS_END();
	if (len > INT_MAX) {
		zend_value_error("cairo_ws_send(): the message is larger than 2 GB");
		RETURN_THROWS();
	}
	int queued = goWSSend(socket, data, (int)len, binary);
	if (queued < 0) {
		RETURN_LONG(queued);
	}
	RETURN_BOOL(queued);
}

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_ws_take, 0, 0, 1)
	ZEND_ARG_INFO(0, socket)
ZEND_END_ARG_INFO()

// cairo_ws_take(int $socket): array|bool takes the next message that the
// client has sent, as [string $data, bool $binary]. false when none has
// arrived (the Go operation http.ws.wait then waits for one), true when the
// client has gone and all that it sent is taken.
static PHP_FUNCTION(cairo_ws_take)
{
	zend_long socket;
	ZEND_PARSE_PARAMETERS_START(1, 1)
		Z_PARAM_LONG(socket)
	ZEND_PARSE_PARAMETERS_END();
	int len = 0, binary = 0;
	char *data = NULL; // Go's copy of the message, ours to free
	int state = goWSTake(socket, &data, &len, &binary);
	if (state <= 0) {
		RETURN_BOOL(state < 0);
	}
	array_init_size(return_value, 2);
	add_next_index_stringl(return_value, data == NULL ? "" : data, (size_t)len);
	add_next_index_bool(return_value, binary);
	free(data);
}

ZEND_BEGIN_ARG_INFO_EX(arginfo_cairo_topic_publish, 0, 0, 4)
	ZEND_ARG_INFO(0, topic)
	ZEND_ARG_INFO(0, data)
	ZEND_ARG_INFO(0, event)
	ZEND_ARG_INFO(0, id)
ZEND_END_ARG_INFO()

// cairo_topic_publish(string $topic, string $data, string $event, string $id): int
// queues a message for the listeners of a topic and returns how many
// listen; -1 when it is not queued, and the Go operation http.topics.publish
// then waits for room, or says what is wrong with the message.
static PHP_FUNCTION(cairo_topic_publish)
{
	char *topic, *data, *event, *id;
	size_t topic_len, data_len, event_len, id_len;
	ZEND_PARSE_PARAMETERS_START(4, 4)
		Z_PARAM_STRING(topic, topic_len)
		Z_PARAM_STRING(data, data_len)
		Z_PARAM_STRING(event, event_len)
		Z_PARAM_STRING(id, id_len)
	ZEND_PARSE_PARAMETERS_END();
	if (topic_len > INT_MAX || data_len > INT_MAX || event_len > INT_MAX || id_len > INT_MAX) {
		RETURN_LONG(-1);
	}
	cairo_publication p = {
		.topic = topic, .data = data, .event = event, .id = id,
		.topic_len = (int)topic_len, .data_len = (int)data_len,
		.event_len = (int)event_len, .id_len = (int)id_len,
	};
	RETURN_LONG(goTopicPublish(&p));
}

static const zend_function_entry cairo_websocket_functions[] = {
	PHP_FE(cairo_topic_publish, arginfo_cairo_topic_publish)
	PHP_FE(cairo_ws_send, arginfo_cairo_ws_send)
	PHP_FE(cairo_ws_take, arginfo_cairo_ws_take)
	PHP_FE_END
};

const cairo_extension cairo_websocket = {
	.functions = cairo_websocket_functions,
};
