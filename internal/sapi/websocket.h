#ifndef CAIRO_WEBSOCKET_H
#define CAIRO_WEBSOCKET_H

#include "extension.h"

// A WebSocket's messages between PHP and Go without a Go operation:
// cairo_ws_send() queues one for the client, and cairo_ws_take() takes one
// that the client has sent; cairo_topic_publish() queues a message for
// the listeners of a topic (websocket.c). websocket.go adds them to the
// SAPI module.
extern const cairo_extension cairo_websocket;

// A message that PHP publishes to a topic, for goTopicPublish.
typedef struct {
	const char *topic, *data, *event, *id;
	int topic_len, data_len, event_len, id_len;
} cairo_publication;

#endif
