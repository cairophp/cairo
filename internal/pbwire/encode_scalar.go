package pbwire

import (
	"bytes"
	"encoding/base64"
	"math"
	"slices"
	"strconv"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/cairophp/cairo/internal/wire"
)

// scalar reads one value that is not a message and returns it as the bytes
// have it: the number of a varint or a fixed-size value, or the data of a
// string. Only the forms that protojson cannot read in two ways are taken:
// a number for a number, digits for a 64-bit integer, a name or a number
// for an enum, padded standard base64 for bytes.
func (e *encoder) scalar(f *field) (num uint64, data []byte, ok bool) {
	tag, ok := e.tag()
	if !ok {
		return 0, nil, false
	}
	switch tag {
	case wire.TagFalse, wire.TagTrue:
		return uint64(tag - wire.TagFalse), nil, f.kind == protoreflect.BoolKind
	case wire.TagInt:
		v, ok := e.u64()
		if !ok {
			return 0, nil, false
		}
		switch i := int64(v); {
		case f.kind == protoreflect.FloatKind:
			return uint64(math.Float32bits(float32(i))), nil, true
		case f.kind == protoreflect.DoubleKind:
			return math.Float64bits(float64(i)), nil, true
		case i < 0:
			num, ok = integer(f.kind, true, -v)
		default:
			num, ok = integer(f.kind, false, v)
		}
		return num, nil, ok
	case wire.TagFloat:
		v, ok := e.u64()
		if !ok {
			return 0, nil, false
		}
		num, ok = float(f.kind, math.Float64frombits(v))
		return num, nil, ok
	case wire.TagString:
		s, ok := e.bytes()
		if !ok {
			return 0, nil, false
		}
		return e.text(f, s)
	}
	return 0, nil, false
}

// integer returns how the bytes have the integer -mag or mag in a field of
// this kind, when it fits one.
func integer(kind protoreflect.Kind, negative bool, mag uint64) (uint64, bool) {
	v := int64(mag)
	if negative {
		v = -v
	}
	fits32 := mag < 1<<31 || negative && mag == 1<<31
	switch kind {
	case protoreflect.Int32Kind, protoreflect.EnumKind:
		return uint64(v), fits32
	case protoreflect.Sint32Kind:
		return protowire.EncodeZigZag(v), fits32
	case protoreflect.Sfixed32Kind:
		return uint64(uint32(v)), fits32
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return mag, !negative && mag <= math.MaxUint32
	case protoreflect.Int64Kind, protoreflect.Sfixed64Kind:
		return uint64(v), mag < 1<<63 || negative && mag == 1<<63
	case protoreflect.Sint64Kind:
		return protowire.EncodeZigZag(v), mag < 1<<63 || negative && mag == 1<<63
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return mag, !negative
	}
	return 0, false
}

// float returns the bits of f in a float or a double field. protojson reads
// the number from its JSON text, and a float32 read from the text of a
// double is not always the double rounded, so one that is not exact goes
// through the same text.
func float(kind protoreflect.Kind, f float64) (uint64, bool) {
	switch {
	case math.IsNaN(f) || math.IsInf(f, 0):
		return 0, false // no JSON form: wire.Unmarshal's error
	case kind == protoreflect.DoubleKind:
		return math.Float64bits(f), true
	case kind != protoreflect.FloatKind:
		return 0, false
	case float64(float32(f)) == f:
		return uint64(math.Float32bits(float32(f))), true
	}
	var buf [32]byte
	text, err := wire.AppendJSONFloat(buf[:0], f)
	if err != nil {
		return 0, false
	}
	g, err := strconv.ParseFloat(string(text), 32)
	return uint64(math.Float32bits(float32(g))), err == nil
}

// text returns what a PHP string is in a field.
func (e *encoder) text(f *field, s []byte) (num uint64, data []byte, ok bool) {
	switch f.kind {
	case protoreflect.StringKind:
		return 0, s, utf8.Valid(s)
	case protoreflect.BytesKind:
		if len(s)%4 != 0 || bytes.ContainsAny(s, "-_") {
			return 0, nil, false // protojson reads these with other alphabets
		}
		e.scratch = slices.Grow(e.scratch[:0], base64.StdEncoding.DecodedLen(len(s)))
		n, err := base64.StdEncoding.Decode(e.scratch[:cap(e.scratch)], s)
		return 0, e.scratch[:n], err == nil
	case protoreflect.EnumKind:
		v, ok := f.values[string(s)]
		return uint64(int64(v)), nil, ok
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		negative, mag, ok := digits(s)
		if !ok {
			return 0, nil, false
		}
		num, ok = integer(f.kind, negative, mag)
		return num, nil, ok
	}
	return 0, nil, false
}

// digits reads an integer written the one way that nothing reads
// differently: a minus sign or none, then digits with no leading zero.
func digits(s []byte) (negative bool, mag uint64, ok bool) {
	if len(s) > 0 && s[0] == '-' {
		negative, s = true, s[1:]
	}
	if len(s) == 0 || len(s) > 20 || s[0] == '0' && (len(s) > 1 || negative) {
		return false, 0, false
	}
	for _, c := range s {
		if c < '0' || c > '9' || mag > (math.MaxUint64-uint64(c-'0'))/10 {
			return false, 0, false
		}
		mag = mag*10 + uint64(c-'0')
	}
	return negative, mag, true
}

// keyNumber returns the number of a map's key, which PHP has as text. The
// bytes of a string key are the key itself.
func keyNumber(kind protoreflect.Kind, key []byte) (uint64, bool) {
	switch kind {
	case protoreflect.StringKind:
		return 0, utf8.Valid(key)
	case protoreflect.BoolKind:
		return protowire.EncodeBool(string(key) == "true"), string(key) == "true" || string(key) == "false"
	}
	negative, mag, ok := digits(key)
	if !ok {
		return 0, false
	}
	return integer(kind, negative, mag)
}
