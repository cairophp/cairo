package sapi

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/cairophp/cairo/internal/core"
	"github.com/cairophp/cairo/internal/grpcserver"
	"github.com/cairophp/cairo/internal/wire"
)

// Streaming gRPC from PHP. Server side: Cairo\Grpc\ServerStream reads and
// writes the stream of the method PHP is serving (grpcserver keeps it).
// Client side: Cairo\Grpc\ClientStream drives a grpc.ClientStream kept here.

type grpcStreamRequest struct {
	grpcCallRequest
	ID int64 `json:"id"`
}

// grpcServerStreamRequest is what a server's stream is asked: the reply to
// send stays PHP's array, which grpcserver converts.
type grpcServerStreamRequest struct {
	ID      int64    `json:"id"`
	Message wire.Raw `json:"message"`
}

type clientStream struct {
	cs     grpc.ClientStream
	method *grpcMethod
	cancel context.CancelFunc
	sendMu sync.Mutex

	// The replies are read ahead of PHP, from its first recv.
	readOnce sync.Once
	ahead    *grpcserver.ReadAhead
}

var (
	clientStreams sync.Map // int64 -> *clientStream
	clientNextID  atomic.Int64
)

func init() {
	registerPHP("grpc.php")
	registerStreamOps(map[string]func(context.Context, grpcServerStreamRequest) map[string]any{
		"grpc.server.recv": grpcServerRecv,
		"grpc.server.send": grpcServerSend,
	})
	registerStreamOps(map[string]func(context.Context, grpcStreamRequest) map[string]any{
		"grpc.stream.open":      grpcStreamOpen,
		"grpc.stream.send":      grpcStreamSend,
		"grpc.stream.closeSend": grpcStreamCloseSend,
		"grpc.stream.recv":      grpcStreamRecv,
		"grpc.stream.close":     grpcStreamClose,
	})
}

func registerStreamOps[Request any](ops map[string]func(context.Context, Request) map[string]any) {
	for name, fn := range ops {
		core.RegisterOp(name, func(ctx context.Context, payload []byte) (any, error) {
			var req Request
			if err := wire.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			return fn(ctx, req), nil
		})
	}
}

func grpcStatusFail(err error) map[string]any {
	st := status.Convert(err)
	return grpcFail(int(st.Code()), st.Message())
}

func grpcServerRecv(ctx context.Context, req grpcServerStreamRequest) map[string]any {
	msgs, eof, err := grpcserver.StreamRecv(ctx, req.ID)
	if err != nil {
		return grpcStatusFail(err)
	}
	if eof {
		return map[string]any{"ok": true, "eof": true}
	}
	return map[string]any{"ok": true, "messages": msgs}
}

func grpcServerSend(_ context.Context, req grpcServerStreamRequest) map[string]any {
	if err := grpcserver.StreamSend(req.ID, req.Message); err != nil {
		return grpcStatusFail(err)
	}
	return map[string]any{"ok": true}
}

// grpcStreamOpen starts a streaming call. The stream is not tied to the
// opening fiber (an async() task may hand it on), but keeps the request's
// deadline, and its own timeout when one is given.
func grpcStreamOpen(ctx context.Context, req grpcStreamRequest) map[string]any {
	m, fail := grpcMethodOf(&req.grpcCallRequest)
	if fail != nil {
		return fail
	}
	md := m.desc
	conn, err := grpcConn(req.Target, req.TLS)
	if err != nil {
		return grpcFail(14, err.Error())
	}
	sctx := context.WithoutCancel(ctx)
	var cancel context.CancelFunc
	if dl, ok := ctx.Deadline(); ok {
		sctx, cancel = context.WithDeadline(sctx, dl)
	} else {
		sctx, cancel = context.WithCancel(sctx)
	}
	if req.Timeout > 0 {
		var c2 context.CancelFunc
		sctx, c2 = context.WithTimeout(sctx, time.Duration(req.Timeout*float64(time.Second)))
		prev := cancel
		cancel = func() { c2(); prev() }
	}
	if len(req.Metadata) > 0 {
		sctx = metadata.NewOutgoingContext(sctx, metadata.MD(lowerKeys(req.Metadata)))
	}
	cs, err := conn.NewStream(sctx, &grpc.StreamDesc{ServerStreams: md.IsStreamingServer(), ClientStreams: md.IsStreamingClient()}, m.path)
	if err != nil {
		cancel()
		return grpcStatusFail(err)
	}
	id := clientNextID.Add(1)
	clientStreams.Store(id, &clientStream{cs: cs, method: m, cancel: cancel})
	return map[string]any{"ok": true, "id": id}
}

func loadClientStream(id int64) (*clientStream, map[string]any) {
	v, ok := clientStreams.Load(id)
	if !ok {
		return nil, grpcFail(1, "the stream is closed") // CANCELLED
	}
	return v.(*clientStream), nil
}

// whileWaiting cancels the stream if the calling fiber's context ends while
// it waits (the request was cancelled or ran out of time).
func whileWaiting(ctx context.Context, st *clientStream, fn func() error) error {
	stop := context.AfterFunc(ctx, st.cancel)
	defer stop()
	return fn()
}

func grpcStreamSend(ctx context.Context, req grpcStreamRequest) map[string]any {
	st, fail := loadClientStream(req.ID)
	if fail != nil {
		return fail
	}
	in, fail := st.method.request(req.Message)
	if fail != nil {
		return fail
	}
	st.sendMu.Lock()
	defer st.sendMu.Unlock()
	err := whileWaiting(ctx, st, func() error { return st.cs.SendMsg(&grpcserver.Frame{Data: in}) })
	if errors.Is(err, io.EOF) {
		// The server ended the call; its status comes with the next recv.
		return map[string]any{"ok": true, "ended": true}
	}
	if err != nil {
		return grpcStatusFail(err)
	}
	return map[string]any{"ok": true}
}

func grpcStreamCloseSend(_ context.Context, req grpcStreamRequest) map[string]any {
	st, fail := loadClientStream(req.ID)
	if fail != nil {
		return fail
	}
	st.sendMu.Lock()
	defer st.sendMu.Unlock()
	if err := st.cs.CloseSend(); err != nil {
		return grpcStatusFail(err)
	}
	return map[string]any{"ok": true}
}

// grpcStreamRecv returns the replies that have arrived, waiting for the
// first when there is none. Once all are taken, a successful call returns
// "eof" with the headers and trailers, and a failed call its status.
func grpcStreamRecv(ctx context.Context, req grpcStreamRequest) map[string]any {
	st, fail := loadClientStream(req.ID)
	if fail != nil {
		return fail
	}
	st.readOnce.Do(func() { st.ahead = grpcserver.NewReadAhead(st.cs.Context(), st.recv) })
	msgs, err := st.ahead.Take(ctx)
	switch {
	case err == nil:
		return map[string]any{"ok": true, "messages": msgs}
	case errors.Is(err, io.EOF):
		header, _ := st.cs.Header()
		return map[string]any{"ok": true, "eof": true, "headers": mdMap(header), "trailers": mdMap(st.cs.Trailer())}
	case ctx.Err() != nil:
		// The request was cancelled or ran out of time while it waited:
		// its stream ends with it.
		st.cancel()
	}
	return grpcStatusFail(err)
}

func (st *clientStream) recv() (wire.Raw, error) {
	var out grpcserver.Frame
	if err := st.cs.RecvMsg(&out); err != nil {
		return nil, err
	}
	return st.method.reply(out.Data)
}

func grpcStreamClose(_ context.Context, req grpcStreamRequest) map[string]any {
	if v, ok := clientStreams.LoadAndDelete(req.ID); ok {
		v.(*clientStream).cancel()
	}
	return map[string]any{"ok": true}
}
