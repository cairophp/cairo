// Package pbwiretest makes random protobuf messages for the tests of pbwire
// and of the gRPC server that uses it: the unit tests compare with
// protojson in Go, the end-to-end test with json_decode() in PHP.
package pbwiretest

import (
	"math"
	"math/rand"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

var (
	someInts   = []int64{0, 1, -1, 2, 127, 128, 300, math.MaxInt32, math.MinInt32, math.MaxUint32, math.MaxInt64, math.MinInt64, 1 << 53, -(1 << 53)}
	someFloats = []float64{0, math.Copysign(0, -1), 1, -1, 1.5, 0.1, 1e-7, 1e-6, 123456.789, 1e15, 1e21, 123456789012345680000,
		1 << 53, 1<<53 + 2, 1 << 62, 1 << 63, -(1 << 63), 1 << 64, math.MaxFloat32, math.MaxFloat64, math.SmallestNonzeroFloat64,
		math.SmallestNonzeroFloat32, 16777216, 3.4e38, math.NaN(), math.Inf(1), math.Inf(-1)}
	someStrings = []string{"", "a", "0", "5", "-5", "05", "true", "héllo ☃", "a\x00b", "line\n\"q\"\\ </tag>", " ", "NaN", "12345678901234567890"}
)

// Int is an integer, an edge of some integer type two times in three.
func Int(r *rand.Rand) int64 {
	if r.Intn(3) == 0 {
		return int64(r.Uint64())
	}
	return someInts[r.Intn(len(someInts))]
}

// Float is a float: an edge, any bits, or a short fraction.
func Float(r *rand.Rand) float64 {
	switch r.Intn(4) {
	case 0:
		return math.Float64frombits(r.Uint64())
	case 1:
		return float64(r.Int63n(1<<40)) / float64(int64(1)<<uint(r.Intn(8)))
	}
	return someFloats[r.Intn(len(someFloats))]
}

// String is valid UTF-8, often one that JSON, PHP or a number could trip on.
func String(r *rand.Rand) string {
	if r.Intn(4) == 0 {
		s := make([]rune, r.Intn(12))
		for i := range s {
			s[i] = rune(r.Intn(0x250))
		}
		return string(s)
	}
	return someStrings[r.Intn(len(someStrings))]
}

// Bytes are up to 8 bytes.
func Bytes(r *rand.Rand) []byte {
	b := make([]byte, r.Intn(9))
	r.Read(b)
	return b
}

// Message is a message of type md with about two fields of three set, the
// messages in them too, down to depth 3.
func Message(r *rand.Rand, md protoreflect.MessageDescriptor, depth int) *dynamicpb.Message {
	m := dynamicpb.NewMessage(md)
	fds := md.Fields()
	for i := 0; i < fds.Len(); i++ {
		fd := fds.Get(i)
		switch {
		case r.Intn(3) == 0:
		case fd.IsList():
			for n := r.Intn(4); n > 0; n-- {
				m.Mutable(fd).List().Append(value(r, fd, depth))
			}
		case fd.IsMap():
			for n := r.Intn(4); n > 0; n-- {
				m.Mutable(fd).Map().Set(value(r, fd.MapKey(), depth).MapKey(), value(r, fd.MapValue(), depth))
			}
		default:
			m.Set(fd, value(r, fd, depth))
		}
	}
	return m
}

func value(r *rand.Rand, fd protoreflect.FieldDescriptor, depth int) protoreflect.Value {
	switch fd.Kind() {
	case protoreflect.BoolKind:
		return protoreflect.ValueOfBool(r.Intn(2) == 0)
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return protoreflect.ValueOfInt32(int32(Int(r)))
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return protoreflect.ValueOfUint32(uint32(Int(r)))
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return protoreflect.ValueOfInt64(Int(r))
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return protoreflect.ValueOfUint64(uint64(Int(r)))
	case protoreflect.FloatKind:
		return protoreflect.ValueOfFloat32(float32(Float(r)))
	case protoreflect.DoubleKind:
		return protoreflect.ValueOfFloat64(Float(r))
	case protoreflect.StringKind:
		return protoreflect.ValueOfString(String(r))
	case protoreflect.BytesKind:
		return protoreflect.ValueOfBytes(Bytes(r))
	case protoreflect.EnumKind:
		return protoreflect.ValueOfEnum([]protoreflect.EnumNumber{0, 1, 2, -1, 77}[r.Intn(5)])
	}
	if depth >= 3 {
		return protoreflect.ValueOfMessage(dynamicpb.NewMessage(fd.Message()))
	}
	return protoreflect.ValueOfMessage(Message(r, fd.Message(), depth+1))
}
