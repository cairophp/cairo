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
	recvMu sync.Mutex

	// Replies go through out to one writer goroutine, so PHP can queue a
	// reply without waiting for it to be written. The first send error
	// stops the writer and is returned by the following sends.
	startOnce sync.Once
	closeMu   sync.RWMutex // held for reading while a reply is queued
	closed    bool
	out       chan *dynamicpb.Message
	done      chan struct{}
	sendErr   atomic.Pointer[error]
}

// sendQueue is how many replies may wait for the writer; a send beyond it
// waits on the fiber's goroutine (StreamSend) until there is room.
const sendQueue = 64

var (
	streams      sync.Map // int64 -> *activeStream
	nextStreamID atomic.Int64
)

func openStream(ss grpc.ServerStream, desc protoreflect.MethodDescriptor) int64 {
	id := nextStreamID.Add(1)
	streams.Store(id, &activeStream{ss: ss, desc: desc})
	return id
}

// closeStream waits until the queued replies are written, so they reach the
// client before the call's status.
func closeStream(id int64) {
	v, ok := streams.LoadAndDelete(id)
	if !ok {
		return
	}
	st := v.(*activeStream)
	st.closeMu.Lock()
	st.closed = true
	st.closeMu.Unlock()
	if st.out != nil {
		close(st.out)
		<-st.done
	}
}

func (st *activeStream) writer() {
	defer close(st.done)
	for m := range st.out {
		if st.sendErr.Load() != nil {
			continue // drain: the client is gone or the stream broke
		}
		if err := st.ss.SendMsg(m); err != nil {
			st.sendErr.Store(&err)
		}
	}
}

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

// StreamSend queues one reply (protojson), waiting for room in the queue
// while the stream is open.
func StreamSend(id int64, msg []byte) error {
	st, out, err := sendable(id, msg)
	if err != nil {
		return err
	}
	defer st.closeMu.RUnlock()
	select {
	case st.out <- out:
		return nil
	case <-st.ss.Context().Done():
		return status.FromContextError(st.ss.Context().Err()).Err()
	}
}

// StreamTrySend queues one reply if there is room, without waiting; queued
// is false when the queue is full and the caller should use StreamSend.
func StreamTrySend(id int64, msg []byte) (queued bool, err error) {
	st, out, err := sendable(id, msg)
	if err != nil {
		return false, err
	}
	defer st.closeMu.RUnlock()
	select {
	case st.out <- out:
		return true, nil
	default:
		return false, nil
	}
}

// sendable decodes msg for stream id and returns with closeMu held for
// reading, so the queue stays open until the caller has queued msg.
func sendable(id int64, msg []byte) (*activeStream, *dynamicpb.Message, error) {
	v, ok := streams.Load(id)
	if !ok {
		return nil, nil, ErrStreamClosed
	}
	st := v.(*activeStream)
	if !st.desc.IsStreamingServer() {
		return nil, nil, errors.New("a client-streaming method replies by returning its message")
	}
	if err := st.sendErr.Load(); err != nil {
		return nil, nil, *err
	}
	out := dynamicpb.NewMessage(st.desc.Output())
	if err := protojson.Unmarshal(msg, out); err != nil {
		return nil, nil, status.Errorf(codes.Internal, "invalid %s: %v", st.desc.Output().FullName(), err)
	}
	st.closeMu.RLock()
	if st.closed {
		st.closeMu.RUnlock()
		return nil, nil, ErrStreamClosed
	}
	st.startOnce.Do(func() {
		st.out = make(chan *dynamicpb.Message, sendQueue)
		st.done = make(chan struct{})
		go st.writer()
	})
	return st, out, nil
}
