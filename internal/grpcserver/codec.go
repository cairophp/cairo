package grpcserver

import (
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/encoding/proto"
	"google.golang.org/grpc/mem"
)

// Frame is a message as its encoded bytes. The methods PHP serves, and the
// calls PHP makes as a client, read and write frames, which a Message
// converts to and from PHP's arrays with no message object in between.
type Frame struct{ Data []byte }

// Codec passes the bytes of a Frame through and leaves every other message
// (health checks, reflection) to grpc's own protobuf codec.
type Codec struct{ proto encoding.CodecV2 }

func NewCodec() Codec { return Codec{encoding.GetCodecV2(proto.Name)} }

func (c Codec) Name() string { return c.proto.Name() }

func (c Codec) Marshal(v any) (mem.BufferSlice, error) {
	if f, ok := v.(*Frame); ok {
		return mem.BufferSlice{mem.SliceBuffer(f.Data)}, nil
	}
	return c.proto.Marshal(v)
}

func (c Codec) Unmarshal(data mem.BufferSlice, v any) error {
	if f, ok := v.(*Frame); ok {
		f.Data = data.Materialize()
		return nil
	}
	return c.proto.Unmarshal(data, v)
}
