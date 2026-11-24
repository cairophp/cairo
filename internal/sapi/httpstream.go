package sapi

/*
#include "http_stream.h"
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"unsafe"

	"github.com/cairophp/cairo/internal/core"
	"github.com/cairophp/cairo/internal/httpserver"
	"github.com/cairophp/cairo/internal/wire"
)

// A streamed HTTP answer as PHP has it (Cairo\Http\Stream, php/http_stream.php):
// the operations that begin it and that wait for room in its queue, and the
// call that queues a piece without waiting (goHTTPSend).
func init() {
	registerPHP("http_stream.php")
	registerC(&C.cairo_http_stream)
	core.RegisterOp("http.stream.open", func(ctx context.Context, payload []byte) (any, error) {
		var req struct {
			Status  int                 `json:"status"`
			Headers map[string][]string `json:"headers"`
		}
		if err := wire.Unmarshal(payload, &req); err != nil {
			return nil, fmt.Errorf("http.stream.open: %w", err)
		}
		return httpserver.OpenStream(ctx, req.Status, req.Headers)
	})
	core.RegisterOp("http.hints", func(ctx context.Context, payload []byte) (any, error) {
		var req struct {
			Status  int                 `json:"status"`
			Headers map[string][]string `json:"headers"`
		}
		if err := wire.Unmarshal(payload, &req); err != nil {
			return nil, fmt.Errorf("http.hints: %w", err)
		}
		return nil, httpserver.Hint(ctx, req.Status, req.Headers)
	})
	// True once the piece is queued, false for a client that is gone.
	core.RegisterOp("http.stream.write", func(ctx context.Context, payload []byte) (any, error) {
		var req struct {
			ID   int64  `json:"id"`
			Data []byte `json:"data"`
		}
		if err := wire.Unmarshal(payload, &req); err != nil {
			return nil, fmt.Errorf("http.stream.write: %w", err)
		}
		err := httpserver.Send(ctx, req.ID, req.Data)
		if errors.Is(err, httpserver.ErrStreamClosed) {
			return nil, err
		}
		return err == nil, nil
	})
}

// goHTTPSend queues one piece of the body of a streamed answer. It runs on
// the PHP thread and never waits: 1 when the piece is queued, 0 when the
// queue is full, -1 when a write to the client has failed, -2 when the
// stream has ended. data is PHP's memory, copied here.
//
//export goHTTPSend
func goHTTPSend(stream C.longlong, data *C.char, n C.int) C.int {
	queued, err := httpserver.TrySend(int64(stream), C.GoBytes(unsafe.Pointer(data), n))
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
