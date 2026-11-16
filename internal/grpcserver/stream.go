package grpcserver

import (
	"encoding/json"
	"errors"
	"io"
	"sync"
	"sync/atomic"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Streaming handlers: while PHP runs a streaming method, its gRPC stream is
// kept here under an id; PHP's Cairo\Grpc\ServerStream reaches it through
// StreamRecv and StreamSend (Go operations run on the handler fiber's
// goroutine, or on an async() task's).

type activeStream struct {
	ss     grpc.ServerStream
	desc   protoreflect.MethodDescriptor
	sendMu sync.Mutex
	recvMu sync.Mutex
}

var (
	streams      sync.Map // int64 -> *activeStream
	nextStreamID atomic.Int64
)

func openStream(ss grpc.ServerStream, desc protoreflect.MethodDescriptor) int64 {
	id := nextStreamID.Add(1)
	streams.Store(id, &activeStream{ss: ss, desc: desc})
	return id
}

func closeStream(id int64) { streams.Delete(id) }

// ErrStreamClosed is returned for a stream whose handler has returned.
var ErrStreamClosed = errors.New("the stream has ended")

// StreamRecv reads the client's next message as protojson; eof is true when
// the client has finished sending.
func StreamRecv(id int64) (msg json.RawMessage, eof bool, err error) {
	v, ok := streams.Load(id)
	if !ok {
		return nil, false, ErrStreamClosed
	}
	st := v.(*activeStream)
	if !st.desc.IsStreamingClient() {
		return nil, false, errors.New("only client-streaming and bidirectional methods receive from the stream")
	}
	st.recvMu.Lock()
	defer st.recvMu.Unlock()
	in := dynamicpb.NewMessage(st.desc.Input())
	if err := st.ss.RecvMsg(in); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, true, nil
		}
		return nil, false, err
	}
	b, err := protojson.MarshalOptions{EmitUnpopulated: true}.Marshal(in)
	return b, false, err
}

// StreamSend sends one reply (protojson) to the client.
func StreamSend(id int64, msg []byte) error {
	v, ok := streams.Load(id)
	if !ok {
		return ErrStreamClosed
	}
	st := v.(*activeStream)
	if !st.desc.IsStreamingServer() {
		return errors.New("a client-streaming method replies by returning its message")
	}
	out := dynamicpb.NewMessage(st.desc.Output())
	if err := protojson.Unmarshal(msg, out); err != nil {
		return status.Errorf(codes.Internal, "invalid %s: %v", st.desc.Output().FullName(), err)
	}
	st.sendMu.Lock()
	defer st.sendMu.Unlock()
	return st.ss.SendMsg(out)
}
