package sapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/cairophp/cairo/internal/grpcserver"
)

// Streaming gRPC from PHP. Server side: Cairo\Grpc\ServerStream reads and
// writes the stream of the method PHP is serving (grpcserver keeps it).
// Client side: Cairo\Grpc\ClientStream drives a grpc.ClientStream kept here.

type grpcStreamRequest struct {
	grpcCallRequest
	ID int64 `json:"id"`
}

type clientStream struct {
	cs     grpc.ClientStream
	desc   protoreflect.MethodDescriptor
	cancel context.CancelFunc
	sendMu sync.Mutex
	recvMu sync.Mutex
}

var (
	clientStreams sync.Map // int64 -> *clientStream
	clientNextID  atomic.Int64
)

func init() {
	ops := map[string]func(context.Context, grpcStreamRequest) map[string]any{
		"grpc.server.recv":      grpcServerRecv,
		"grpc.server.send":      grpcServerSend,
		"grpc.server.trySend":   grpcServerTrySend,
		"grpc.stream.open":      grpcStreamOpen,
		"grpc.stream.send":      grpcStreamSend,
		"grpc.stream.closeSend": grpcStreamCloseSend,
		"grpc.stream.recv":      grpcStreamRecv,
		"grpc.stream.close":     grpcStreamClose,
	}
	for name, fn := range ops {
		fn := fn
		registerOp(name, func(ctx context.Context, payload []byte) (any, error) {
			var req grpcStreamRequest
			if err := json.Unmarshal(payload, &req); err != nil {
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

func grpcServerRecv(_ context.Context, req grpcStreamRequest) map[string]any {
	msg, eof, err := grpcserver.StreamRecv(req.ID)
	if err != nil {
		return grpcStatusFail(err)
	}
	if eof {
		return map[string]any{"ok": true, "eof": true}
	}
	return map[string]any{"ok": true, "message": msg}
}

func grpcServerSend(_ context.Context, req grpcStreamRequest) map[string]any {
	if err := grpcserver.StreamSend(req.ID, req.Message); err != nil {
		return grpcStatusFail(err)
	}
	return map[string]any{"ok": true}
}

// grpcServerTrySend runs on the PHP thread (it never waits): queued is
// false when the reply queue is full and PHP should use grpc.server.send.
func grpcServerTrySend(_ context.Context, req grpcStreamRequest) map[string]any {
	queued, err := grpcserver.StreamTrySend(req.ID, req.Message)
	if err != nil {
		return grpcStatusFail(err)
	}
	return map[string]any{"ok": true, "queued": queued}
}

// grpcStreamOpen starts a streaming call. The stream is not tied to the
// opening fiber (an async() task may hand it on), but keeps the request's
// deadline, and its own timeout when one is given.
func grpcStreamOpen(ctx context.Context, req grpcStreamRequest) map[string]any {
	files, err := grpcRegistry(req.Protos, req.Descriptors)
	if err != nil {
		return grpcFail(13, err.Error())
	}
	name := strings.TrimPrefix(req.Method, "/")
	svc, meth, _ := strings.Cut(name, "/")
	d, err := files.FindDescriptorByName(protoreflect.FullName(svc))
	sd, isSvc := d.(protoreflect.ServiceDescriptor)
	if err != nil || !isSvc {
		return grpcFail(12, "service "+svc+" is not in the client's descriptors")
	}
	md := sd.Methods().ByName(protoreflect.Name(meth))
	if md == nil {
		return grpcFail(12, "method "+meth+" is not in "+svc)
	}
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
	cs, err := conn.NewStream(sctx, &grpc.StreamDesc{ServerStreams: md.IsStreamingServer(), ClientStreams: md.IsStreamingClient()}, "/"+name)
	if err != nil {
		cancel()
		return grpcStatusFail(err)
	}
	id := clientNextID.Add(1)
	clientStreams.Store(id, &clientStream{cs: cs, desc: md, cancel: cancel})
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
	in := dynamicpb.NewMessage(st.desc.Input())
	if len(req.Message) > 0 && string(req.Message) != "null" && string(req.Message) != "[]" {
		if err := protojson.Unmarshal(req.Message, in); err != nil {
			return grpcFail(3, "request does not match "+string(st.desc.Input().FullName())+": "+err.Error())
		}
	}
	st.sendMu.Lock()
	defer st.sendMu.Unlock()
	err := whileWaiting(ctx, st, func() error { return st.cs.SendMsg(in) })
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

// grpcStreamRecv reads the next reply; at the end of a successful call it
// returns "eof" with the headers and trailers, and a failed call returns its
// status.
func grpcStreamRecv(ctx context.Context, req grpcStreamRequest) map[string]any {
	st, fail := loadClientStream(req.ID)
	if fail != nil {
		return fail
	}
	st.recvMu.Lock()
	defer st.recvMu.Unlock()
	out := dynamicpb.NewMessage(st.desc.Output())
	err := whileWaiting(ctx, st, func() error { return st.cs.RecvMsg(out) })
	if errors.Is(err, io.EOF) {
		header, _ := st.cs.Header()
		return map[string]any{"ok": true, "eof": true, "headers": mdMap(header), "trailers": mdMap(st.cs.Trailer())}
	}
	if err != nil {
		res := grpcStatusFail(err)
		res["trailers"] = mdMap(st.cs.Trailer())
		return res
	}
	msg, err := protojson.MarshalOptions{EmitUnpopulated: true}.Marshal(out)
	if err != nil {
		return grpcFail(13, err.Error())
	}
	return map[string]any{"ok": true, "message": json.RawMessage(msg)}
}

func grpcStreamClose(_ context.Context, req grpcStreamRequest) map[string]any {
	if v, ok := clientStreams.LoadAndDelete(req.ID); ok {
		v.(*clientStream).cancel()
	}
	return map[string]any{"ok": true}
}
