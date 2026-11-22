package grpcserver

import (
	"encoding/json"
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/cairophp/cairo/internal/pbwire"
	"github.com/cairophp/cairo/internal/wire"
)

// Message converts the messages of one type between protobuf's bytes and
// the array PHP has for them, which is protojson's. pbwire does it directly
// for the common cases; a type or a value it leaves alone goes through a
// message object and JSON text, and so does every error, so that what a
// client and a handler see is protojson's either way.
type Message struct {
	desc protoreflect.MessageDescriptor
	plan *pbwire.Plan // nil for a type that only protojson converts
}

func NewMessage(desc protoreflect.MessageDescriptor) Message {
	return Message{desc, pbwire.Compile(desc)}
}

// ToPHP appends the array of an encoded message as a value in
// internal/wire's form.
func (m Message) ToPHP(b, data []byte) ([]byte, error) {
	if m.plan != nil {
		if out, ok := m.plan.AppendWire(b, data); ok {
			return out, nil
		}
	}
	msg := dynamicpb.NewMessage(m.desc)
	if err := unmarshal(data, msg); err != nil {
		return b, err
	}
	text, err := protojson.MarshalOptions{EmitUnpopulated: true}.Marshal(msg)
	if err != nil {
		return b, err
	}
	return wire.AppendJSON(b, text), nil
}

// unmarshal is proto.Unmarshal for bytes from the network. protobuf-go
// (1.36.12) panics on a map entry whose key comes again with another wire
// type; that is a message to refuse like any other damaged one.
func unmarshal(data []byte, msg proto.Message) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("proto: %v", v)
		}
	}()
	return proto.Unmarshal(data, msg)
}

// FromPHP returns the encoding of the message that PHP gave as value.
func (m Message) FromPHP(value wire.Raw) ([]byte, error) {
	if m.plan != nil {
		if out, ok := m.plan.AppendProto(make([]byte, 0, len(value)), value); ok {
			return out, nil
		}
	}
	var text json.RawMessage
	if err := wire.Unmarshal(value, &text); err != nil {
		return nil, err
	}
	msg := dynamicpb.NewMessage(m.desc)
	if err := protojson.Unmarshal(text, msg); err != nil {
		return nil, err
	}
	return proto.Marshal(msg)
}
