package pbwire_test

import (
	"fmt"
	"math"
	"math/rand"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/cairophp/cairo/internal/grpcserver"
	"github.com/cairophp/cairo/internal/pbwire"
	"github.com/cairophp/cairo/internal/pbwire/pbwiretest"
)

// checkWire holds AppendWire to its contract for one encoding: bytes that
// proto.Unmarshal refuses are not converted, and what is converted is, for
// PHP, what protojson's text is. It returns whether data was converted.
func checkWire(tb testing.TB, md protoreflect.MessageDescriptor, plan *pbwire.Plan, data []byte) bool {
	tb.Helper()
	prefix := []byte("kept")
	got, ok := plan.AppendWire(prefix, data)
	if string(got[:len(prefix)]) != "kept" || !ok && len(got) != len(prefix) {
		tb.Fatalf("% x: what was there before is not kept", data)
	}
	m := dynamicpb.NewMessage(md)
	if err := unmarshal(data, m); err != nil {
		if ok {
			tb.Fatalf("% x: converted, but proto.Unmarshal says %v", data, err)
		}
		return false
	}
	if !ok {
		return false
	}
	text, err := protojson.MarshalOptions{EmitUnpopulated: true}.Marshal(m)
	if err != nil {
		tb.Fatalf("% x: converted, but protojson says %v", data, err)
	}
	if want, have := phpFromJSON(tb, text), phpFromWire(tb, got[len(prefix):]); want != have {
		tb.Fatalf("% x:\nprotojson: %s\npbwire:    %s", data, want, have)
	}
	return true
}

// unmarshal is proto.Unmarshal, with its panic as one more way to refuse:
// protobuf-go 1.36.12 has one for a map entry whose key comes again with
// another wire type.
func unmarshal(data []byte, m proto.Message) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("panic: %v", v)
		}
	}()
	return proto.Unmarshal(data, m)
}

func TestAppendWireRandomMessages(t *testing.T) {
	md, plan := kinds(t, "All")
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 1000; i++ {
		data, err := proto.Marshal(pbwiretest.Message(r, md, 0))
		if err != nil {
			t.Fatal(err)
		}
		if !checkWire(t, md, plan, data) {
			t.Fatalf("% x: a message that proto.Marshal wrote was left to protojson", data)
		}
	}
}

// Encodings that proto.Marshal does not write but must be read: a message
// in parts, fields given again, damaged and cut bytes, unknown fields.
func TestAppendWireRandomBytes(t *testing.T) {
	md, plan := kinds(t, "All")
	r := rand.New(rand.NewSource(2))
	converted := 0
	for i := 0; i < 3000; i++ {
		data, _ := proto.Marshal(pbwiretest.Message(r, md, 1))
		switch r.Intn(4) {
		case 0: // two messages one after the other are the two merged
			more, _ := proto.Marshal(pbwiretest.Message(r, md, 2))
			data = append(data, more...)
		case 1:
			for n := r.Intn(3) + 1; n > 0 && len(data) > 0; n-- {
				data[r.Intn(len(data))] ^= byte(1 << uint(r.Intn(8)))
			}
		case 2:
			data = data[:r.Intn(len(data)+1)]
		case 3:
			data = protowire.AppendVarint(protowire.AppendTag(data, protowire.Number(r.Intn(2000)+1), protowire.VarintType), r.Uint64())
		}
		if checkWire(t, md, plan, data) {
			converted++
		}
	}
	if converted < 1000 {
		t.Fatalf("only %d of 3000 encodings were converted", converted)
	}
}

func TestAppendWireEncodings(t *testing.T) {
	md, plan := kinds(t, "All")
	varint := func(b []byte, num protowire.Number, v uint64) []byte {
		return protowire.AppendVarint(protowire.AppendTag(b, num, protowire.VarintType), v)
	}
	delimited := func(b []byte, num protowire.Number, v []byte) []byte {
		return protowire.AppendBytes(protowire.AppendTag(b, num, protowire.BytesType), v)
	}
	leaf := delimited(nil, 1, []byte("x"))
	cases := map[string]struct {
		data      []byte
		converted bool
	}{
		"empty":                      {nil, true},
		"a scalar twice":             {varint(varint(nil, 3, 1), 3, 2), true},
		"packed":                     {delimited(nil, 23, []byte{1, 2, 0xac, 2}), true},
		"not packed":                 {varint(varint(nil, 23, 1), 23, 2), true},
		"both":                       {varint(delimited(nil, 23, []byte{1}), 23, 2), true},
		"packed, cut":                {delimited(nil, 23, []byte{0x80}), false},
		"packed fixed":               {delimited(nil, 27, []byte{1, 0, 0, 0, 2, 0, 0, 0}), true},
		"another wire type":          {protowire.AppendFixed32(protowire.AppendTag(nil, 3, protowire.Fixed32Type), 7), true},
		"a message in parts":         {delimited(delimited(nil, 17, leaf), 17, varint(nil, 2, 5)), true},
		"an empty message":           {delimited(nil, 17, nil), true},
		"oneof, the last wins":       {delimited(varint(nil, 61, 4), 62, []byte("s")), true},
		"oneof, message dropped":     {varint(delimited(nil, 63, leaf), 61, 4), false},
		"optional zero":              {varint(nil, 51, 0), true},
		"bad UTF-8":                  {delimited(nil, 14, []byte("\xff")), false},
		"bad UTF-8, replaced":        {delimited(delimited(nil, 14, []byte("\xff")), 14, []byte("ok")), false},
		"map, no key":                {delimited(nil, 41, varint(nil, 2, 9)), true},
		"map, no value":              {delimited(nil, 43, varint(nil, 1, 9)), true},
		"map, a key twice":           {delimited(delimited(nil, 42, varint(delimited(nil, 2, []byte("a")), 1, 7)), 42, varint(delimited(nil, 2, []byte("b")), 1, 7)), true},
		"map, a key lost":            {delimited(nil, 42, protowire.AppendFixed64(protowire.AppendTag(varint(nil, 1, 7), 1, protowire.Fixed64Type), 9)), false},
		"map, a key found again":     {delimited(nil, 42, varint(protowire.AppendFixed64(protowire.AppendTag(varint(nil, 1, 7), 1, protowire.Fixed64Type), 9), 1, 8)), true},
		"map, a key of another type": {delimited(nil, 42, protowire.AppendFixed64(protowire.AppendTag(nil, 1, protowire.Fixed64Type), 9)), true},
		"map, a message dropped":     {delimited(delimited(nil, 43, varint(delimited(nil, 2, leaf), 1, 7)), 43, varint(nil, 1, 7)), false},
		"sint32":                     {varint(nil, 7, 0xffffffff), true},
		"int32, 64 bits":             {varint(nil, 3, math.MaxUint64-5), true},
		"unknown enum":               {varint(nil, 16, 77), true},
		"a group":                    {protowire.AppendTag(protowire.AppendTag(nil, 500, protowire.StartGroupType), 500, protowire.EndGroupType), true},
		"field 0":                    {[]byte{0, 0}, false},
		"far number":                 {varint(nil, 1000, 9), true},
	}
	for name, c := range cases {
		if got := checkWire(t, md, plan, c.data); got != c.converted {
			t.Errorf("%s: converted is %v, want %v", name, got, c.converted)
		}
	}
}

func TestCompileLeavesOtherTypesToProtojson(t *testing.T) {
	files, err := grpcserver.CompileProtos([]string{"../codegen/testdata/shop.proto"})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[protoreflect.FullName]bool{"shop.v1.Item": true, "shop.v1.Order": false} {
		d, err := files.FindDescriptorByName(name)
		if err != nil {
			t.Fatal(err)
		}
		if got := pbwire.Compile(d.(protoreflect.MessageDescriptor)) != nil; got != want {
			t.Errorf("%s: has a plan is %v, want %v (Order has a Timestamp)", name, got, want)
		}
	}
}

func FuzzAppendWire(f *testing.F) {
	md, plan := kinds(f, "All")
	r := rand.New(rand.NewSource(3))
	for i := 0; i < 20; i++ {
		data, _ := proto.Marshal(pbwiretest.Message(r, md, 2))
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		checkWire(t, md, plan, data)
	})
}

func BenchmarkAppendWire(b *testing.B) {
	md, plan := kinds(b, "Leaf")
	m := dynamicpb.NewMessage(md)
	m.Set(md.Fields().ByName("name"), protoreflect.ValueOfString("Hello, world"))
	m.Set(md.Fields().ByName("n"), protoreflect.ValueOfInt32(42))
	data, _ := proto.Marshal(m)
	var buf []byte
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		buf, _ = plan.AppendWire(buf[:0], data)
	}
}
