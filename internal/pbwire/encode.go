package pbwire

import (
	"encoding/binary"
	"sync"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/cairophp/cairo/internal/wire"
)

// AppendProto appends to b the encoding of the message that PHP gave as
// the array in value. ok is false, and b is returned as it came, for an
// array that protojson would refuse (an unknown field, a value of the wrong
// type) and for one in a form this package leaves to protojson (a number as
// text where a number is expected, an object that PHP encoded itself, a
// float with no JSON form), so the caller's protojson path is what decides
// and reports both.
func (p *Plan) AppendProto(b, value []byte) (out []byte, ok bool) {
	e := encoders.Get().(*encoder)
	e.in, e.i = value, 0
	out, ok = e.message(b, p, 0)
	ok = ok && e.i == len(value)
	clear(e.keys[:cap(e.keys)])
	e.in, e.seen, e.keys = nil, e.seen[:0], e.keys[:0]
	if cap(e.scratch) > pooledScratch {
		e.scratch = nil
	}
	encoders.Put(e)
	if !ok {
		return b, false
	}
	return out, true
}

// pooledScratch is the most room for decoded bytes that an encoder keeps
// for the next message.
const pooledScratch = 64 << 10

var encoders = sync.Pool{New: func() any { return new(encoder) }}

type encoder struct {
	in      []byte
	i       int
	seen    []bool   // per message being written: the fields given, then the oneofs set
	keys    [][]byte // the keys of the maps being written
	scratch []byte   // decoded base64
}

// The reader of internal/wire's form. Every length is checked against what
// is left, as wire.Unmarshal does.

func (e *encoder) tag() (byte, bool) {
	if e.i >= len(e.in) {
		return 0, false
	}
	e.i++
	return e.in[e.i-1], true
}

func (e *encoder) u64() (uint64, bool) {
	if len(e.in)-e.i < 8 {
		return 0, false
	}
	e.i += 8
	return binary.LittleEndian.Uint64(e.in[e.i-8:]), true
}

func (e *encoder) bytes() ([]byte, bool) {
	if len(e.in)-e.i < 4 {
		return nil, false
	}
	n := int(binary.LittleEndian.Uint32(e.in[e.i:]))
	e.i += 4
	if len(e.in)-e.i < n {
		return nil, false
	}
	e.i += n
	return e.in[e.i-n : e.i], true
}

// count reads how many entries the list or the map with this tag has.
func (e *encoder) count(tag byte, depth int) (int, bool) {
	if t, ok := e.tag(); !ok || t != tag || depth >= wire.MaxDepth || len(e.in)-e.i < 4 {
		return 0, false
	}
	n := int(binary.LittleEndian.Uint32(e.in[e.i:]))
	e.i += 4
	return n, n <= len(e.in)-e.i
}

// message writes the fields of one message, in the order PHP's array has
// them. protojson's rules: a field goes by its JSON name or its proto name
// and is given once, null leaves it unset, and a oneof has one member.
func (e *encoder) message(b []byte, p *Plan, depth int) ([]byte, bool) {
	n, ok := e.count(wire.TagMap, depth)
	if !ok {
		return b, false
	}
	base := len(e.seen)
	e.seen = append(e.seen, p.unseen...)
	for ; n > 0; n-- {
		name, ok := e.bytes()
		i, known := p.names[string(name)]
		if !ok || !known || e.seen[base+int(i)] {
			return b, false
		}
		e.seen[base+int(i)] = true
		if e.i < len(e.in) && e.in[e.i] == wire.TagNull {
			e.i++
			continue
		}
		f := &p.fields[i]
		switch f.card {
		case list:
			b, ok = e.list(b, f, depth+1)
		case mapping:
			b, ok = e.mapping(b, f, depth+1)
		default:
			if o := base + len(p.fields) + int(f.oneof); f.oneof >= 0 {
				if e.seen[o] {
					return b, false
				}
				e.seen[o] = true
			}
			b, ok = e.value(b, f, depth+1, f.presence)
		}
		if !ok {
			return b, false
		}
	}
	e.seen = e.seen[:base]
	return b, true
}

// value writes a field's tag and one value. A zero is left out unless the
// field keeps track of being set.
func (e *encoder) value(b []byte, f *field, depth int, zeroToo bool) ([]byte, bool) {
	if f.kind == protoreflect.MessageKind {
		b, size := delimited(protowire.AppendTag(b, f.num, protowire.BytesType))
		b, ok := e.message(b, f.msg, depth)
		return endDelimited(b, size), ok
	}
	num, data, ok := e.scalar(f)
	if !ok || !zeroToo && num == 0 && len(data) == 0 {
		return b, ok
	}
	return appendValue(protowire.AppendTag(b, f.num, f.wtype), f.wtype, num, data), true
}

func appendValue(b []byte, typ protowire.Type, num uint64, data []byte) []byte {
	switch typ {
	case protowire.VarintType:
		return protowire.AppendVarint(b, num)
	case protowire.Fixed32Type:
		return protowire.AppendFixed32(b, uint32(num))
	case protowire.Fixed64Type:
		return protowire.AppendFixed64(b, num)
	}
	return protowire.AppendBytes(b, data)
}

// delimited leaves one byte for the length of what follows, which
// endDelimited fills in: most messages are shorter than 128 bytes, and a
// longer one is moved up to make room.
func delimited(b []byte) ([]byte, int) {
	return append(b, 0), len(b)
}

func endDelimited(b []byte, size int) []byte {
	n := len(b) - size - 1
	if n < 0x80 {
		b[size] = byte(n)
		return b
	}
	more := protowire.SizeVarint(uint64(n)) - 1
	b = append(b, make([]byte, more)...)
	copy(b[size+1+more:], b[size+1:size+1+n])
	protowire.AppendVarint(b[:size], uint64(n))
	return b
}

func (e *encoder) list(b []byte, f *field, depth int) ([]byte, bool) {
	n, ok := e.count(wire.TagList, depth)
	if !ok || n == 0 {
		return b, ok
	}
	if !f.packed {
		for ; n > 0 && ok; n-- {
			b, ok = e.value(b, f, depth+1, true)
		}
		return b, ok
	}
	b, size := delimited(protowire.AppendTag(b, f.num, protowire.BytesType))
	for ; n > 0; n-- {
		num, data, ok := e.scalar(f)
		if !ok {
			return b, false
		}
		b = appendValue(b, f.wtype, num, data)
	}
	return endDelimited(b, size), true
}

// mapping writes each entry of a map as a message of its key and its value.
func (e *encoder) mapping(b []byte, f *field, depth int) ([]byte, bool) {
	n, ok := e.count(wire.TagMap, depth)
	if !ok {
		return b, false
	}
	// A key given twice is refused by protojson. PHP's arrays cannot have
	// one, other bytes can: a small map compares its keys one by one.
	var large map[string]struct{}
	if n > linearKeys {
		large = map[string]struct{}{}
	}
	base := len(e.keys)
	for ; n > 0; n-- {
		key, ok := e.bytes()
		if _, twice := large[string(key)]; !ok || twice || large == nil && e.has(base, key) {
			return b, false
		}
		if large != nil {
			large[string(key)] = struct{}{}
		} else {
			e.keys = append(e.keys, key)
		}
		num, ok := keyNumber(f.mapKey.kind, key)
		if !ok {
			return b, false
		}
		var size int
		b, size = delimited(protowire.AppendTag(b, f.num, protowire.BytesType))
		b = appendValue(protowire.AppendTag(b, 1, f.mapKey.wtype), f.mapKey.wtype, num, key)
		if b, ok = e.value(b, f.mapValue, depth+1, true); !ok {
			return b, false
		}
		b = endDelimited(b, size)
	}
	e.keys = e.keys[:base]
	return b, true
}

// linearKeys is up to how many keys of a map are compared one by one.
const linearKeys = 16

func (e *encoder) has(base int, key []byte) bool {
	for _, k := range e.keys[base:] {
		if string(k) == string(key) {
			return true
		}
	}
	return false
}
