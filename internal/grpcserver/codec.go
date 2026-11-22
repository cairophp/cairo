package grpcserver

import (
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/encoding/proto"
	"google.golang.org/grpc/mem"
)

// frame is a message as its encoded bytes. The methods PHP serves read and
// write frames, which pbwire converts to and from PHP's arrays with no
// message object in between.
type frame struct{ data []byte }

// codec passes the bytes of a frame through and leaves every other message
// (health checks, reflection) to grpc's own protobuf codec.
type codec struct{ proto encoding.CodecV2 }

func newCodec() codec { return codec{encoding.GetCodecV2(proto.Name)} }

func (c codec) Name() string { return c.proto.Name() }

func (c codec) Marshal(v any) (mem.BufferSlice, error) {
	if f, ok := v.(*frame); ok {
		return mem.BufferSlice{mem.SliceBuffer(f.data)}, nil
	}
	return c.proto.Marshal(v)
}

func (c codec) Unmarshal(data mem.BufferSlice, v any) error {
	if f, ok := v.(*frame); ok {
		f.data = data.Materialize()
		return nil
	}
	return c.proto.Unmarshal(data, v)
}
