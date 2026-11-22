package test

import (
	"context"
	"errors"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"regexp"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/cairophp/cairo/internal/grpcserver"
	"github.com/cairophp/cairo/internal/pbwire/pbwiretest"
)

// startKinds serves testdata/grpc-kinds and returns a connection to it and
// the service's descriptor.
func startKinds(t *testing.T) (*grpc.ClientConn, protoreflect.ServiceDescriptor) {
	t.Helper()
	addr := freeAddr()
	cmd := exec.Command(cairoBin, "serve", "testdata/grpc-kinds/server.php", "--threads", "2")
	cmd.Env = append(os.Environ(), "CAIRO_GRPC="+addr)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		_ = cmd.Wait()
	})
	if err := waitFor(addr, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	files, err := grpcserver.CompileProtos([]string{"../internal/pbwire/testdata/kinds.proto"})
	if err != nil {
		t.Fatal(err)
	}
	d, err := files.FindDescriptorByName("kinds.Kinds")
	if err != nil {
		t.Fatal(err)
	}
	return conn, d.(protoreflect.ServiceDescriptor)
}

// Messages with every kind of field cross to PHP and back without a
// change: PHP gets the array that json_decode() makes of protojson's text
// for the message, value for value and type for type, and the array it
// returns is the message again. Unary calls and a stream.
func TestGRPCMessageKinds(t *testing.T) {
	conn, svc := startKinds(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	all := svc.Methods().ByName("Echo").Input()
	check := svc.Methods().ByName("Check")
	each, err := conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}, "/kinds.Kinds/EchoEach")
	if err != nil {
		t.Fatal(err)
	}
	r := rand.New(rand.NewSource(7))
	for i := 0; i < 300; i++ {
		m := pbwiretest.Message(r, all, 1)
		text, err := protojson.MarshalOptions{EmitUnpopulated: true}.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}

		compare := dynamicpb.NewMessage(check.Input())
		compare.Set(check.Input().Fields().ByName("message"), protoreflect.ValueOfMessage(m))
		compare.Set(check.Input().Fields().ByName("json"), protoreflect.ValueOfString(string(text)))
		verdict := dynamicpb.NewMessage(check.Output())
		if err := conn.Invoke(ctx, "/kinds.Kinds/Check", compare, verdict); err != nil {
			t.Fatalf("Check %s: %v", text, err)
		}
		if !verdict.Get(check.Output().Fields().ByName("same")).Bool() {
			t.Fatalf("PHP got another array than json_decode() of %s:\n%s", text, verdict.Get(check.Output().Fields().ByName("got")).String())
		}

		// JSON's -0 is PHP's int 0, so a negative zero does not come back.
		m = dynamicpb.NewMessage(all)
		if err := protojson.Unmarshal(negativeZero.ReplaceAll(text, []byte("${1}0${2}")), m); err != nil {
			t.Fatal(err)
		}
		echo := dynamicpb.NewMessage(all)
		if err := conn.Invoke(ctx, "/kinds.Kinds/Echo", m, echo); err != nil {
			t.Fatalf("Echo %s: %v", text, err)
		}
		if !proto.Equal(m, echo) {
			t.Fatalf("Echo: sent %s\ngot %s", text, protojson.Format(echo))
		}

		echo = dynamicpb.NewMessage(all)
		if err := each.SendMsg(m); err != nil {
			t.Fatalf("EchoEach %s: %v", text, err)
		}
		if err := each.RecvMsg(echo); err != nil {
			t.Fatalf("EchoEach %s: %v", text, err)
		}
		if !proto.Equal(m, echo) {
			t.Fatalf("EchoEach: sent %s\ngot %s", text, protojson.Format(echo))
		}
	}
	_ = each.CloseSend()
	if err := each.RecvMsg(dynamicpb.NewMessage(all)); !errors.Is(err, io.EOF) {
		t.Fatalf("EchoEach ended with %v", err)
	}

	// A message nested deeper than the binary form goes crosses as JSON
	// text, both ways.
	deep := dynamicpb.NewMessage(all)
	for i, m := 0, deep; i < 80; i++ {
		m.Set(all.Fields().ByName("f_int32"), protoreflect.ValueOfInt32(int32(i)))
		m = m.Mutable(all.Fields().ByName("f_self")).Message().(*dynamicpb.Message)
	}
	echo := dynamicpb.NewMessage(all)
	if err := conn.Invoke(ctx, "/kinds.Kinds/Echo", deep, echo); err != nil || !proto.Equal(deep, echo) {
		t.Fatalf("Echo of 80 nested messages: %v", err)
	}
}

var negativeZero = regexp.MustCompile(`([:\[,])-0([,}\]])`)

// bytesCodec sends and receives a message as the bytes it is given.
type bytesCodec struct{}

func (bytesCodec) Name() string { return "proto" }

func (bytesCodec) Marshal(v any) (mem.BufferSlice, error) {
	return mem.BufferSlice{mem.SliceBuffer(*v.(*[]byte))}, nil
}

func (bytesCodec) Unmarshal(data mem.BufferSlice, v any) error {
	*v.(*[]byte) = data.Materialize()
	return nil
}

var _ encoding.CodecV2 = bytesCodec{}

// Bytes that are no message end their call with INTERNAL, on a unary call
// and inside a stream, and the server goes on. One of them is a map entry
// that protobuf-go panics on: its key comes again with another wire type.
func TestGRPCDamagedMessage(t *testing.T) {
	conn, svc := startKinds(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	entry := protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), "k")
	entry = protowire.AppendFixed64(protowire.AppendTag(entry, 1, protowire.Fixed64Type), 9)
	damaged := map[string][]byte{
		"cut short":      {0x0a, 0x05, 'c', 'u', 't'},
		"not UTF-8":      protowire.AppendString(protowire.AppendTag(nil, 14, protowire.BytesType), "\xff"),
		"a key lost":     protowire.AppendBytes(protowire.AppendTag(nil, 41, protowire.BytesType), entry),
		"a key lost, in": protowire.AppendBytes(protowire.AppendTag(nil, 18, protowire.BytesType), protowire.AppendBytes(protowire.AppendTag(nil, 41, protowire.BytesType), entry)),
	}
	raw := grpc.ForceCodecV2(bytesCodec{})
	for name, data := range damaged {
		var reply []byte
		if err := conn.Invoke(ctx, "/kinds.Kinds/Echo", &data, &reply, raw); status.Code(err) != codes.Internal {
			t.Errorf("%s, unary: %v", name, err)
		}
		each, err := conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}, "/kinds.Kinds/EchoEach", raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := each.SendMsg(&data); err != nil {
			t.Fatalf("%s, stream: %v", name, err)
		}
		if err := each.RecvMsg(&reply); status.Code(err) != codes.Internal {
			t.Errorf("%s, stream: %v", name, err)
		}
	}
	all := svc.Methods().ByName("Echo").Input()
	m := dynamicpb.NewMessage(all)
	m.Set(all.Fields().ByName("f_string"), protoreflect.ValueOfString("still here"))
	echo := dynamicpb.NewMessage(all)
	if err := conn.Invoke(ctx, "/kinds.Kinds/Echo", m, echo); err != nil || !proto.Equal(m, echo) {
		t.Fatalf("after the damaged messages: %v", err)
	}
}
