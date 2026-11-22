package grpcserver

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/cairophp/cairo/internal/wire"
)

// A type that pbwire converts and one that it leaves to protojson (Order
// has a Timestamp) both make the round trip through PHP's array.
func TestMessageRoundTrip(t *testing.T) {
	for name, value := range map[protoreflect.FullName]map[string]any{
		"shop.v1.Item":  {"sku": "a-1", "qty": "7", "blob": "aGk="},
		"shop.v1.Order": {"id": "7", "createdAt": "2026-11-22T10:00:00Z", "items": []any{map[string]any{"sku": "a-1"}}},
	} {
		m := newMessage(describe(t, "../codegen/testdata/shop.proto", name))
		if direct := m.plan != nil; direct != (name == "shop.v1.Item") {
			t.Errorf("%s: converted directly is %v", name, direct)
		}
		data, err := m.fromPHP(wire.Encode(value))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		array, err := m.toPHP(nil, data)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		again, err := m.fromPHP(array)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		a, b := dynamicpb.NewMessage(m.desc), dynamicpb.NewMessage(m.desc)
		if proto.Unmarshal(data, a) != nil || proto.Unmarshal(again, b) != nil || !proto.Equal(a, b) || len(data) == 0 {
			t.Errorf("%s: %x came back as %x", name, data, again)
		}
	}
}

// What protojson refuses is an error either way, in protojson's words.
func TestMessageErrors(t *testing.T) {
	m := newMessage(describe(t, "../codegen/testdata/shop.proto", "shop.v1.Item"))
	if _, err := m.fromPHP(wire.Encode(map[string]any{"nobody": 1})); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Errorf("an unknown field: %v", err)
	}
	if _, err := m.fromPHP(wire.Encode(map[string]any{"sku": "\xff"})); err == nil {
		t.Error("a string that is not UTF-8 was encoded")
	}
	if _, err := m.toPHP(nil, []byte{0x0a, 0x05, 'c', 'u', 't'}); err == nil {
		t.Error("a message cut short was decoded")
	}
}

// protobuf-go panics on a map entry whose key comes again with another wire
// type. Such bytes from a client are an error, not the end of the server.
func TestMessageDamagedMapEntry(t *testing.T) {
	entry := protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), "k")
	entry = protowire.AppendFixed64(protowire.AppendTag(entry, 1, protowire.Fixed64Type), 9)
	data := protowire.AppendBytes(protowire.AppendTag(nil, 3, protowire.BytesType), entry) // Order.tags
	for _, name := range []protoreflect.FullName{"shop.v1.Order", "kinds.Tagged"} {
		path := "../codegen/testdata/shop.proto"
		if name == "kinds.Tagged" {
			path = "../pbwire/testdata/kinds.proto"
		}
		m := newMessage(describe(t, path, name))
		if _, err := m.toPHP(nil, data); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
}

// What one message costs between the bytes on the wire and PHP's array,
// converted directly and through protojson.
func BenchmarkMessage(b *testing.B) {
	desc := describe(b, "../../examples/greeter/proto/helloworld.proto", "helloworld.HelloReply")
	value := wire.Encode(map[string]any{"message": "bench 57", "remaining": 0})
	for name, m := range map[string]message{"direct": newMessage(desc), "protojson": {desc: desc}} {
		data, err := m.fromPHP(value)
		if err != nil {
			b.Fatal(err)
		}
		b.Run("fromPHP/"+name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := m.fromPHP(value); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run("toPHP/"+name, func(b *testing.B) {
			b.ReportAllocs()
			var buf []byte
			for i := 0; i < b.N; i++ {
				if buf, err = m.toPHP(buf[:0], data); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
