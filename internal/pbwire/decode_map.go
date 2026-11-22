package pbwire

import (
	"bytes"
	"encoding/binary"
	"sort"
	"strconv"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/cairophp/cairo/internal/wire"
)

// entry is one entry of a map field in the bytes. The number of its key is
// the key's value, not its encoding.
type entry struct {
	key, value       occurrence
	hasKey, hasValue bool
}

// mapping writes a map field as protojson does: the entries in the order
// of their keys, and the last of those with the same key.
func (d *decoder) mapping(b []byte, f *field, occs occurrences, depth int) ([]byte, bool) {
	if depth >= wire.MaxDepth {
		return b, false
	}
	base := len(d.entries)
	for at := occs.first; at != 0; at = d.occs[at-1].next {
		e, ok := d.entry(f, d.occs[at-1])
		if !ok {
			return b, false
		}
		d.entries = append(d.entries, e)
	}
	d.sorted = byKey{d, d.entries[base:], f.mapKey.kind}
	sort.Stable(&d.sorted)
	sorted := d.sorted // a map inside a value sorts too
	b, count := container(b, wire.TagMap)
	n, ok := 0, true
	for i, e := range sorted.entries {
		if i+1 < len(sorted.entries) && sorted.compare(i, i+1) == 0 {
			// Replaced by the next one, but proto.Unmarshal would have read
			// its message, and may have refused it.
			if f.mapValue.kind == protoreflect.MessageKind {
				return b, false
			}
			continue
		}
		n++
		b = d.mapKey(b, f.mapKey.kind, e.key)
		if f.mapValue.kind != protoreflect.MessageKind {
			b = d.scalar(b, f.mapValue, e.value)
			continue
		}
		var from occurrences
		mark := len(d.occs)
		if e.hasValue {
			d.occs = append(d.occs, e.value)
			from = occurrences{int32(mark + 1), int32(mark + 1)}
		}
		if b, ok = d.message(b, f.mapValue.msg, from, depth+1); !ok {
			return b, false
		}
		d.occs = d.occs[:mark]
	}
	binary.LittleEndian.PutUint32(b[count:], uint32(n))
	d.entries = d.entries[:base]
	return b, true
}

// entry reads one entry as proto.Unmarshal does: the key and the value are
// fields 1 and 2, the last of each counts, and one that is missing has its
// zero value.
func (d *decoder) entry(f *field, from occurrence) (e entry, ok bool) {
	pos := int(from.num >> 32)
	b := d.msg[:pos+int(uint32(from.num))]
	lostKey := false
	for pos < len(b) {
		num, typ, n := protowire.ConsumeTag(b[pos:])
		if n < 0 || num > protowire.MaxValidNumber {
			return e, false
		}
		pos += n
		part := f.mapKey
		if num == 2 {
			part = f.mapValue
		}
		if num > 2 || typ != part.wtype {
			if n = protowire.ConsumeFieldValue(num, typ, b[pos:]); n < 0 {
				return e, false
			}
			pos += n
			// protobuf-go (1.36.12) forgets the key it has when the key
			// comes again with another wire type, and then panics.
			lostKey = lostKey || num == 1 && e.hasKey
			continue
		}
		o, n := consume(typ, b, pos)
		if n < 0 || part.kind == protoreflect.StringKind && !utf8.Valid(d.data(o)) {
			return e, false
		}
		pos += n
		switch {
		case num == 1:
			e.key, e.hasKey, lostKey = o, true, false
		case e.hasValue && part.kind == protoreflect.MessageKind:
			return e, false // two parts of one message, which protobuf merges
		default:
			e.value, e.hasValue = o, true
		}
	}
	e.key.num = keyValue(f.mapKey.kind, e.key.num)
	return e, !lostKey
}

// keyValue decodes the number of a key; a string's stays where it is.
func keyValue(kind protoreflect.Kind, v uint64) uint64 {
	switch kind {
	case protoreflect.BoolKind:
		return protowire.EncodeBool(v != 0)
	case protoreflect.Int32Kind, protoreflect.Sfixed32Kind:
		return uint64(int32(v))
	case protoreflect.Sint32Kind:
		return uint64(int32(protowire.DecodeZigZag(v & 0xffffffff)))
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return uint64(uint32(v))
	case protoreflect.Sint64Kind:
		return uint64(protowire.DecodeZigZag(v))
	}
	return v
}

func signed(kind protoreflect.Kind) bool {
	switch kind {
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return true
	}
	return false
}

// mapKey writes a key, which is text in JSON whatever its type.
func (d *decoder) mapKey(b []byte, kind protoreflect.Kind, key occurrence) []byte {
	b = append(b, 0, 0, 0, 0)
	start := len(b)
	switch {
	case kind == protoreflect.StringKind:
		b = append(b, d.data(key)...)
	case kind == protoreflect.BoolKind:
		b = strconv.AppendBool(b, key.num != 0)
	case signed(kind):
		b = strconv.AppendInt(b, int64(key.num), 10)
	default:
		b = strconv.AppendUint(b, key.num, 10)
	}
	binary.LittleEndian.PutUint32(b[start-4:], uint32(len(b)-start))
	return b
}

// byKey sorts the entries of a map by key as protojson does: false before
// true, numbers by value, strings by their bytes.
type byKey struct {
	d       *decoder
	entries []entry
	kind    protoreflect.Kind
}

func (s *byKey) Len() int           { return len(s.entries) }
func (s *byKey) Swap(i, j int)      { s.entries[i], s.entries[j] = s.entries[j], s.entries[i] }
func (s *byKey) Less(i, j int) bool { return s.compare(i, j) < 0 }

func (s *byKey) compare(i, j int) int {
	a, b := s.entries[i].key, s.entries[j].key
	switch {
	case s.kind == protoreflect.StringKind:
		return bytes.Compare(s.d.data(a), s.d.data(b))
	case signed(s.kind) && int64(a.num) != int64(b.num):
		if int64(a.num) < int64(b.num) {
			return -1
		}
		return 1
	case a.num < b.num:
		return -1
	case a.num > b.num:
		return 1
	}
	return 0
}
