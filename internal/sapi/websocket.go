package sapi

/*
#include <stdlib.h>
#include "websocket.h"
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unsafe"

	"github.com/cairophp/cairo/internal/core"
	"github.com/cairophp/cairo/internal/httpserver"
	"github.com/cairophp/cairo/internal/wire"
)

// A WebSocket and the topics as PHP has them (Cairo\Http\WebSocket and
// Topic, php/websocket.php): the operations that wait or that happen once
// per connection, and the two calls that move a message without waiting
// (goWSSend, goWSTake).
func init() {
	registerPHP("websocket.php", `Cairo\Http\WebSocket`, `Cairo\Http\Message`, `Cairo\Http\Topic`)
	registerC(&C.cairo_websocket)
	// [id, subprotocol], or null when the handshake was refused.
	core.RegisterOp("http.ws.open", func(ctx context.Context, payload []byte) (any, error) {
		var opts httpserver.SocketOptions
		if err := wire.Unmarshal(payload, &opts); err != nil {
			return nil, fmt.Errorf("http.ws.open: %w", err)
		}
		id, protocol, err := httpserver.OpenSocket(ctx, opts, core.Logger())
		if err != nil || id == 0 {
			return nil, err
		}
		return []any{id, protocol}, nil
	})
	// True once the message is queued, false for a client that is gone.
	core.RegisterOp("http.ws.send", func(ctx context.Context, payload []byte) (any, error) {
		var req struct {
			ID     int64  `json:"id"`
			Data   []byte `json:"data"`
			Binary bool   `json:"binary"`
		}
		if err := wire.Unmarshal(payload, &req); err != nil {
			return nil, fmt.Errorf("http.ws.send: %w", err)
		}
		err := httpserver.SendSocket(ctx, req.ID, httpserver.Message(req.Binary, req.Data))
		if errors.Is(err, httpserver.ErrStreamClosed) {
			return nil, err
		}
		return err == nil, nil
	})
	core.RegisterOp("http.ws.wait", func(ctx context.Context, payload []byte) (any, error) {
		var req socketTopics
		if err := wire.Unmarshal(payload, &req); err != nil {
			return nil, fmt.Errorf("http.ws.wait: %w", err)
		}
		return nil, httpserver.WaitSocket(ctx, req.ID)
	})
	core.RegisterOp("http.ws.close", func(_ context.Context, payload []byte) (any, error) {
		var req struct {
			ID     int64  `json:"id"`
			Code   int    `json:"code"`
			Reason string `json:"reason"`
		}
		if err := wire.Unmarshal(payload, &req); err != nil {
			return nil, fmt.Errorf("http.ws.close: %w", err)
		}
		httpserver.CloseSocket(req.ID, req.Code, req.Reason)
		return nil, nil
	})
	core.RegisterOp("http.ws.subscribe", func(_ context.Context, payload []byte) (any, error) {
		var req socketTopics
		if err := wire.Unmarshal(payload, &req); err != nil {
			return nil, fmt.Errorf("http.ws.subscribe: %w", err)
		}
		return nil, httpserver.SubscribeSocket(req.ID, req.Topics)
	})
	core.RegisterOp("http.ws.unsubscribe", func(_ context.Context, payload []byte) (any, error) {
		var req socketTopics
		if err := wire.Unmarshal(payload, &req); err != nil {
			return nil, fmt.Errorf("http.ws.unsubscribe: %w", err)
		}
		httpserver.UnsubscribeSocket(req.ID, req.Topics)
		return nil, nil
	})
	registerTopicOps()
}

type socketTopics struct {
	ID     int64    `json:"id"`
	Topics []string `json:"topics"`
}

func registerTopicOps() {
	// How many clients listen to the topic. PHP comes here when the direct
	// call (goTopicPublish) had no room, or to be told what is wrong.
	core.RegisterOp("http.topics.publish", func(ctx context.Context, payload []byte) (any, error) {
		var req struct {
			Topic string `json:"topic"`
			Data  []byte `json:"data"`
			Event string `json:"event"`
			ID    string `json:"id"`
		}
		if err := wire.Unmarshal(payload, &req); err != nil {
			return nil, fmt.Errorf("http.topics.publish: %w", err)
		}
		return httpserver.Publish(ctx, req.Topic, req.Data, req.Event, req.ID)
	})
	core.RegisterOp("http.topics.listen", func(ctx context.Context, payload []byte) (any, error) {
		var req struct {
			Topics    []string            `json:"topics"`
			Headers   map[string][]string `json:"headers"`
			KeepAlive float64             `json:"keep_alive"`
		}
		if err := wire.Unmarshal(payload, &req); err != nil {
			return nil, fmt.Errorf("http.topics.listen: %w", err)
		}
		keepAlive := time.Duration(req.KeepAlive * float64(time.Second))
		return nil, httpserver.ListenTopics(ctx, req.Topics, req.Headers, keepAlive)
	})
}

// goWSSend queues one message for the client of a WebSocket. It runs on
// the PHP thread and never waits: 1 when the message is queued, 0 when the
// queue is full, -1 when a write to the client has failed, -2 when the
// socket has ended. data is PHP's memory, copied here.
//
//export goWSSend
func goWSSend(socket C.longlong, data *C.char, n C.int, binary C.bool) C.int {
	msg := httpserver.Message(bool(binary), unsafe.Slice((*byte)(unsafe.Pointer(data)), int(n)))
	queued, err := httpserver.TrySendSocket(int64(socket), msg)
	switch {
	case errors.Is(err, httpserver.ErrStreamClosed):
		return -2
	case err != nil:
		return -1
	case queued:
		return 1
	}
	return 0
}

// goWSTake hands PHP the next message that the client of a WebSocket has
// sent: 1 with *data a copy that the caller frees, 0 when none has arrived,
// -1 when the client has gone and all is taken. It runs on the PHP thread
// and never waits.
//
//export goWSTake
func goWSTake(socket C.longlong, data **C.char, n *C.int, binary *C.int) C.int {
	msg, over := httpserver.TakeSocket(int64(socket))
	switch {
	case msg == nil && over:
		return -1
	case msg == nil:
		return 0
	}
	body := msg[1:]
	if len(body) > 0 {
		*data = (*C.char)(C.CBytes(body))
	}
	*n = C.int(len(body))
	if httpserver.IsBinary(msg) {
		*binary = 1
	}
	return 1
}

// goTopicPublish queues a message for the listeners of a topic. It runs on
// the PHP thread and never waits: how many listen, or -1 when the message
// is not queued (no room, or it is not valid) and the operation
// http.topics.publish is to be used. p is PHP's memory, copied here.
//
//export goTopicPublish
func goTopicPublish(p *C.cairo_publication) C.int {
	if p.data_len > httpserver.MaxPublication || max(p.topic_len, p.event_len, p.id_len) > httpserver.MaxTopicName {
		return -1 // nothing of it is copied: the operation says what is wrong
	}
	listening, queued, err := httpserver.TryPublish(
		C.GoStringN(p.topic, p.topic_len), C.GoBytes(unsafe.Pointer(p.data), p.data_len),
		C.GoStringN(p.event, p.event_len), C.GoStringN(p.id, p.id_len))
	if err != nil || !queued {
		return -1
	}
	return C.int(listening)
}
