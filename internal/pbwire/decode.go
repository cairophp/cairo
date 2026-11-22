package pbwire

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"math"
	"strconv"
	"sync"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/cairophp/cairo/internal/wire"
)

// AppendWire appends to b the array PHP gets for the encoded message msg.
// ok is false, and b is returned as it came, for bytes that proto.Unmarshal
// does not take or that this package leaves to protojson.
//
// A message is read in two steps, because its array has every field in the
// order of the declaration while the bytes have them in any order, in parts
// and more than once: the bytes are scanned into the occurrences of each
// field, and the fields are then written from those.
func (p *Plan) AppendWire(b, msg []byte) (out []byte, ok bool) {
	if len(msg) > math.MaxInt32 {
		return b, false
	}
	d := decoders.Get().(*decoder)
	d.msg = msg
	d.occs = append(d.occs[:0], occurrence{num: uint64(len(msg)), typ: protowire.BytesType})
	out, ok = d.message(b, p, occurrences{1, 1}, 0)
	d.msg, d.fields, d.entries, d.sorted = nil, d.fields[:0], d.entries[:0], byKey{}
	if cap(d.occs) <= pooledOccurrences {
		decoders.Put(d)
	}
	if !ok {
		return b, false
	}
	return out, true
}

// pooledOccurrences is the most occurrences a decoder may have room for
// and still be kept for the next message: 1 MB of them.
const pooledOccurrences = 1 << 16

var decoders = sync.Pool{New: func() any { return new(decoder) }}

// decoder holds what the messages being read need, each as a stack: a
// nested message uses the end of each slice and gives it back.
type decoder struct {
	msg     []byte // the whole message
	occs    []occurrence
	fields  []occurrences
	entries []entry
	sorted  byKey
}

// occurrence is one value of a field in the bytes.
type occurrence struct {
	num  uint64 // a varint or a fixed-size value; of a length-delimited one, where it starts in msg (high half) and its length
	next int32  // the field's next occurrence: its index in occs plus one, or 0
	typ  protowire.Type
}

// data is the value of a length-delimited occurrence.
func (d *decoder) data(o occurrence) []byte {
	return d.msg[o.num>>32:][:uint32(o.num)]
}

// occurrences are those of one field, as indexes in occs plus one; the zero
// value is a field that does not occur.
type occurrences struct{ first, last int32 }

// message writes the map of the message whose encoding the occurrences in
// from hold: more than one for a message field that the bytes have in
// parts, which protobuf merges.
func (d *decoder) message(b []byte, p *Plan, from occurrences, depth int) ([]byte, bool) {
	if depth >= wire.MaxDepth {
		return b, false
	}
	base, mark := len(d.fields), len(d.occs)
	d.fields = append(d.fields, p.unset...)
	for o := from.first; o != 0; o = d.occs[o-1].next {
		if !d.scan(p, base, d.occs[o-1]) {
			return b, false
		}
		if o == from.last {
			break
		}
	}
	b, count := container(b, wire.TagMap)
	n, ok := 0, true
	for i := range p.fields {
		f, occs := &p.fields[i], d.fields[base+i]
		if f.card == single && occs.last == 0 && f.unset == unsetAbsent {
			continue
		}
		n++
		b = append(b, f.key...)
		switch {
		case f.card == list:
			b, ok = d.list(b, f, occs, depth+1)
		case f.card == mapping:
			b, ok = d.mapping(b, f, occs, depth+1)
		case occs.last == 0 && f.unset == unsetNull:
			b = wire.AppendNull(b)
		case f.kind == protoreflect.MessageKind:
			b, ok = d.message(b, f.msg, occs, depth+1)
		case occs.last == 0:
			b = d.scalar(b, f, occurrence{})
		default:
			b = d.scalar(b, f, d.occs[occs.last-1])
		}
		if !ok {
			return b, false
		}
	}
	binary.LittleEndian.PutUint32(b[count:], uint32(n))
	d.fields, d.occs = d.fields[:base], d.occs[:mark]
	return b, true
}

// container starts a list or a map and returns where its count goes, which
// is known once the entries are written.
func container(b []byte, tag byte) ([]byte, int) {
	return append(b, tag, 0, 0, 0, 0), len(b) + 1
}

// scan adds the fields of one encoding of a message to the occurrences of
// the message whose fields start at base. It takes what proto.Unmarshal
// takes: a field it does not know, or with another wire type than its own,
// is passed over.
func (d *decoder) scan(p *Plan, base int, from occurrence) bool {
	pos := int(from.num >> 32)
	b := d.msg[:pos+int(uint32(from.num))]
	for pos < len(b) {
		num, typ, n := protowire.ConsumeTag(b[pos:])
		if n < 0 || num > protowire.MaxValidNumber {
			return false
		}
		pos += n
		i, f := p.find(num)
		if f == nil || typ != f.wtype && !(typ == protowire.BytesType && f.card == list) {
			if n = protowire.ConsumeFieldValue(num, typ, b[pos:]); n < 0 {
				return false
			}
			pos += n
			continue
		}
		o, n := consume(typ, b, pos)
		if n < 0 || f.kind == protoreflect.StringKind && !utf8.Valid(d.data(o)) {
			return false
		}
		pos += n
		if f.oneof >= 0 && !d.clearOneof(p, base, i) {
			return false
		}
		d.occs = append(d.occs, o)
		at := int32(len(d.occs))
		if occs := &d.fields[base+i]; occs.last == 0 {
			*occs = occurrences{at, at}
		} else {
			d.occs[occs.last-1].next = at
			occs.last = at
		}
	}
	return true
}

// consume reads the value at b[pos:]; n is its size, negative when it is
// malformed.
func consume(typ protowire.Type, b []byte, pos int) (o occurrence, n int) {
	o.typ = typ
	switch typ {
	case protowire.VarintType:
		o.num, n = protowire.ConsumeVarint(b[pos:])
	case protowire.Fixed32Type:
		var v uint32
		v, n = protowire.ConsumeFixed32(b[pos:])
		o.num = uint64(v)
	case protowire.Fixed64Type:
		o.num, n = protowire.ConsumeFixed64(b[pos:])
	default:
		var v []byte
		v, n = protowire.ConsumeBytes(b[pos:])
		o.num = uint64(pos+n-len(v))<<32 | uint64(len(v))
	}
	return o, n
}

// clearOneof forgets the other members of the oneof that field i belongs
// to: the last member in the bytes is the one that is set. A message among
// them is left to protojson, since proto.Unmarshal would still have read it
// and may have refused it.
func (d *decoder) clearOneof(p *Plan, base, i int) bool {
	for j := range p.fields {
		if j == i || p.fields[j].oneof != p.fields[i].oneof || d.fields[base+j].last == 0 {
			continue
		}
		if p.fields[j].kind == protoreflect.MessageKind {
			return false
		}
		d.fields[base+j] = occurrences{}
	}
	return true
}

func (d *decoder) list(b []byte, f *field, occs occurrences, depth int) ([]byte, bool) {
	if depth >= wire.MaxDepth {
		return b, false
	}
	b, count := container(b, wire.TagList)
	n, ok := 0, true
	for at := occs.first; at != 0; at = d.occs[at-1].next {
		o := d.occs[at-1]
		switch {
		case f.kind == protoreflect.MessageKind:
			b, ok = d.message(b, f.msg, occurrences{at, at}, depth+1)
			n++
		case o.typ != f.wtype:
			// Packed: the elements one after another in one value.
			for packed := d.data(o); len(packed) > 0; n++ {
				e, size := consume(f.wtype, packed, 0)
				if size < 0 {
					return b, false
				}
				packed = packed[size:]
				b = d.scalar(b, f, e)
			}
		default:
			b = d.scalar(b, f, o)
			n++
		}
		if !ok {
			return b, false
		}
	}
	binary.LittleEndian.PutUint32(b[count:], uint32(n))
	return b, true
}

// scalar writes one value that is not a message, as protojson's JSON
// has it once PHP decoded that: a 64-bit integer is a string of digits,
// bytes are base64, an enum is the name of its value.
func (d *decoder) scalar(b []byte, f *field, o occurrence) []byte {
	switch f.kind {
	case protoreflect.BoolKind:
		return wire.AppendBool(b, o.num != 0)
	case protoreflect.Int32Kind, protoreflect.Sfixed32Kind:
		return wire.AppendInt(b, int64(int32(o.num)))
	case protoreflect.Sint32Kind:
		return wire.AppendInt(b, int64(int32(protowire.DecodeZigZag(o.num&math.MaxUint32))))
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return wire.AppendInt(b, int64(uint32(o.num)))
	case protoreflect.Int64Kind, protoreflect.Sfixed64Kind:
		return appendDigits(b, int64(o.num), false)
	case protoreflect.Sint64Kind:
		return appendDigits(b, protowire.DecodeZigZag(o.num), false)
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return appendDigits(b, int64(o.num), true)
	case protoreflect.FloatKind:
		return appendFloat(b, float64(math.Float32frombits(uint32(o.num))), 32)
	case protoreflect.DoubleKind:
		return appendFloat(b, math.Float64frombits(o.num), 64)
	case protoreflect.EnumKind:
		if name, ok := f.enum[protoreflect.EnumNumber(int32(o.num))]; ok {
			return wire.AppendString(b, name)
		}
		return wire.AppendInt(b, int64(int32(o.num)))
	case protoreflect.BytesKind:
		data := d.data(o)
		b = binary.LittleEndian.AppendUint32(append(b, wire.TagString), uint32(base64.StdEncoding.EncodedLen(len(data))))
		return base64.StdEncoding.AppendEncode(b, data)
	}
	return wire.AppendBytes(b, d.data(o)) // a string
}

// appendDigits writes a 64-bit integer as the string of its digits.
func appendDigits(b []byte, v int64, unsigned bool) []byte {
	b = append(b, wire.TagString, 0, 0, 0, 0)
	start := len(b)
	if unsigned {
		b = strconv.AppendUint(b, uint64(v), 10)
	} else {
		b = strconv.AppendInt(b, v, 10)
	}
	binary.LittleEndian.PutUint32(b[start-4:], uint32(len(b)-start))
	return b
}

// appendFloat writes what PHP's json_decode() makes of the number that
// protojson writes for f: an int when the text has neither a fraction nor an
// exponent and fits one, else a float with the value of the text, which for
// a float32 is not the float32's own. The values that JSON has no number
// for are strings.
func appendFloat(b []byte, f float64, bits int) []byte {
	switch {
	case math.IsNaN(f):
		return wire.AppendString(b, "NaN")
	case math.IsInf(f, 1):
		return wire.AppendString(b, "Infinity")
	case math.IsInf(f, -1):
		return wire.AppendString(b, "-Infinity")
	case bits == 64 && f != math.Trunc(f):
		return wire.AppendFloat(b, f)
	case bits == 64 && math.Abs(f) < 1<<53:
		return wire.AppendInt(b, int64(f))
	}
	// protojson's choice of format (encoding/json's).
	format := byte('f')
	if abs := math.Abs(f); abs != 0 {
		if bits == 64 && (abs < 1e-6 || abs >= 1e21) || bits == 32 && (float32(abs) < 1e-6 || float32(abs) >= 1e21) {
			format = 'e'
		}
	}
	var buf [32]byte
	text := strconv.AppendFloat(buf[:0], f, format, -1, bits)
	if format == 'f' && bytes.IndexByte(text, '.') < 0 {
		if i, err := strconv.ParseInt(string(text), 10, 64); err == nil {
			return wire.AppendInt(b, i)
		}
	}
	g, _ := strconv.ParseFloat(string(text), 64)
	return wire.AppendFloat(b, g)
}
