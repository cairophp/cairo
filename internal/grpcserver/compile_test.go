package grpcserver

import (
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

func message(tb testing.TB, path string, name protoreflect.FullName) protoreflect.MessageDescriptor {
	tb.Helper()
	files, err := CompileProtos([]string{path})
	if err != nil {
		tb.Fatal(err)
	}
	d, err := files.FindDescriptorByName(name)
	if err != nil {
		tb.Fatal(err)
	}
	return d.(protoreflect.MessageDescriptor)
}

// A file that imports a well-known type compiles, and its messages still
// read and write that type as protojson does.
func TestCompileProtosWithImports(t *testing.T) {
	desc := message(t, "../codegen/testdata/shop.proto", "shop.v1.Order")
	in := dynamicpb.NewMessage(desc)
	const text = `{"id":"7", "createdAt":"2026-11-22T10:00:00Z"}`
	if err := protojson.Unmarshal([]byte(text), in); err != nil {
		t.Fatal(err)
	}
	raw, err := proto.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	out := dynamicpb.NewMessage(desc)
	if err := proto.Unmarshal(raw, out); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(in, out) {
		t.Errorf("round trip changed the message: %v, want %v", out, in)
	}
}

// What one reply costs between PHP's JSON and the bytes on the wire.
func BenchmarkReplyFromJSON(b *testing.B) {
	desc := message(b, "../../examples/greeter/proto/helloworld.proto", "helloworld.HelloReply")
	text := []byte(`{"message":"bench 57","remaining":0}`)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		out := dynamicpb.NewMessage(desc)
		if err := protojson.Unmarshal(text, out); err != nil {
			b.Fatal(err)
		}
		if _, err := proto.Marshal(out); err != nil {
			b.Fatal(err)
		}
	}
}
