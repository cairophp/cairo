package grpcserver

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cairophp/cairo/internal/duplex"
	"github.com/cairophp/cairo/internal/wire"
)

// Streaming handlers: while PHP runs a streaming method, its gRPC stream is
// kept here under an id; PHP's Cairo\Grpc\ServerStream reaches it through
// StreamRecv and StreamSend (Go operations run on the handler fiber's
// goroutine, or on an async() task's).

type activeStream struct {
	ss     grpc.ServerStream
	method method

	// The client's messages are read ahead of PHP, from its first recv.
	readOnce sync.Once
	ahead    *duplex.ReadAhead[wire.Raw]

	// Replies wait here for their writer, so that the client gets them
	// in order and before the call's status.
	sends *duplex.SendQueue
}

var (
	streams      sync.Map // int64 -> *activeStream
	nextStreamID atomic.Int64
)

func openStream(ss grpc.ServerStream, m method) int64 {
	id := nextStreamID.Add(1)
	sends := duplex.NewSendQueue(func(reply []byte) error { return ss.SendMsg(&Frame{reply}) }, nil)
	streams.Store(id, &activeStream{ss: ss, method: m, sends: sends})
	return id
}

// closeStream waits until the queued replies are written, so they reach the
// client before the call's status.
func closeStream(id int64) {
	if v, ok := streams.LoadAndDelete(id); ok {
		// A reply that could not be written has nobody to tell: its
		// client is gone.
		_ = v.(*activeStream).sends.Close()
	}
}

// ErrStreamClosed is returned for a stream whose handler has returned.
var ErrStreamClosed = duplex.ErrClosed

// CtxStatus is err as a gRPC status when it is how a context ended, which
// is what the queues of a stream return for a caller that stopped waiting.
func CtxStatus(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return status.FromContextError(err).Err()
	}
	return err
}

// StreamRecv returns the client's messages that have arrived, each as PHP's
// array for it, and waits for the first when there is none; eof is true when
// the client has finished sending and all were taken.
func StreamRecv(ctx context.Context, id int64) (msgs []wire.Raw, eof bool, err error) {
	v, ok := streams.Load(id)
	if !ok {
		return nil, false, ErrStreamClosed
	}
	st := v.(*activeStream)
	if !st.method.desc.IsStreamingClient() {
		return nil, false, errors.New("only client-streaming and bidirectional methods receive from the stream")
	}
	st.readOnce.Do(func() { st.ahead = duplex.NewReadAhead(st.ss.Context(), st.recv) })
	msgs, err = st.ahead.Take(ctx)
	if errors.Is(err, io.EOF) {
		return nil, true, nil
	}
	return msgs, false, CtxStatus(err)
}

func (st *activeStream) recv() (wire.Raw, error) {
	var in Frame
	if err := st.ss.RecvMsg(&in); err != nil {
		return nil, err
	}
	msg, err := st.method.in.ToPHP(nil, in.Data)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "grpc: failed to unmarshal the received message: %v", err)
	}
	return msg, nil
}

// StreamSend queues one reply, PHP's array for it, waiting for room in the
// queue while the stream is open.
func StreamSend(id int64, msg wire.Raw) error {
	st, out, err := sendable(id, msg)
	if err != nil {
		return err
	}
	return CtxStatus(st.sends.Put(st.ss.Context(), out))
}

// StreamTrySend queues one reply if there is room, without waiting; queued
// is false when the queue is full and the caller should use StreamSend.
func StreamTrySend(id int64, msg wire.Raw) (queued bool, err error) {
	st, out, err := sendable(id, msg)
	if err != nil {
		return false, err
	}
	return st.sends.TryPut(out)
}

// sendable returns the encoding of msg, a reply of stream id.
func sendable(id int64, msg wire.Raw) (*activeStream, []byte, error) {
	v, ok := streams.Load(id)
	if !ok {
		return nil, nil, ErrStreamClosed
	}
	st := v.(*activeStream)
	if !st.method.desc.IsStreamingServer() {
		return nil, nil, errors.New("a client-streaming method replies by returning its message")
	}
	if err := st.sends.Err(); err != nil {
		return nil, nil, err
	}
	out, err := st.method.out.FromPHP(msg)
	if err != nil {
		return nil, nil, status.Errorf(codes.Internal, "invalid %s: %v", st.method.desc.Output().FullName(), err)
	}
	return st, out, nil
}
