// Package pbwire converts a protobuf message between its encoded bytes and
// the array PHP has for it, in internal/wire's form, with no message object
// and no JSON text in between.
//
// The array is protojson's: AppendWire gives what PHP's json_decode() makes
// of protojson.Marshal (EmitUnpopulated) for the message, and AppendProto
// gives bytes for the message that protojson.Unmarshal reads from the
// array's JSON text. Neither tries to cover everything protojson does: for
// a message type, an encoding or a value outside the common forms they
// answer ok=false, and the caller goes through protojson itself, so what a
// client and a handler see never depends on which way a message went.
package pbwire

import (
	"encoding/binary"
	"strings"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Plan is what both directions need to know of one message type, worked out
// once from its descriptor.
type Plan struct {
	fields []field          // in the order they are declared
	dense  []int32          // field number -> index+1, for the low numbers
	sparse map[int32]int32  // the same for the others
	names  map[string]int32 // JSON name and proto name -> index
	oneofs int              // how many oneofs the message has
	unset  []occurrences    // len(fields) zero values
	unseen []bool           // len(fields)+oneofs zero values
}

// How often a field's value occurs.
const (
	single = iota
	list
	mapping
)

// What an unset single field is in PHP's array.
const (
	unsetDefault = iota // the type's zero value
	unsetNull           // a message
	unsetAbsent         // a member of a oneof, or an optional field
)

type field struct {
	num      protowire.Number
	key      []byte // the name as a key in internal/wire's form
	kind     protoreflect.Kind
	wtype    protowire.Type
	card     uint8
	unset    uint8
	presence bool  // a zero value is still written
	packed   bool  // a list that is written as one length-delimited value
	oneof    int32 // which oneof of the message it belongs to, or -1
	msg      *Plan // of a message
	enum     map[protoreflect.EnumNumber]string
	values   map[string]protoreflect.EnumNumber
	mapKey   *field
	mapValue *field
}

// denseNumbers is how many field numbers are looked up in a slice.
const denseNumbers = 64

// Compile returns the plan of a message type, or nil when the type is one
// that only protojson converts: it is not proto3, or it reaches a
// google.protobuf type (Timestamp, Struct, Any and the others have JSON
// forms of their own).
func Compile(md protoreflect.MessageDescriptor) *Plan {
	c := compiler{plans: map[protoreflect.FullName]*Plan{}}
	p := c.message(md)
	if c.unsupported {
		return nil
	}
	return p
}

type compiler struct {
	plans       map[protoreflect.FullName]*Plan // also of the types being compiled: a message can hold itself
	unsupported bool
}

func (c *compiler) supported(d protoreflect.Descriptor) bool {
	ok := d.ParentFile().Syntax() == protoreflect.Proto3 && !strings.HasPrefix(string(d.FullName()), "google.protobuf.")
	if !ok {
		c.unsupported = true
	}
	return ok
}

func (c *compiler) message(md protoreflect.MessageDescriptor) *Plan {
	if p, ok := c.plans[md.FullName()]; ok {
		return p
	}
	p := &Plan{names: map[string]int32{}, sparse: map[int32]int32{}, oneofs: md.Oneofs().Len()}
	c.plans[md.FullName()] = p
	if !c.supported(md) {
		return p
	}
	fds := md.Fields()
	p.fields = make([]field, fds.Len())
	p.unset = make([]occurrences, fds.Len())
	p.unseen = make([]bool, fds.Len()+p.oneofs)
	for i := 0; i < fds.Len(); i++ {
		fd := fds.Get(i)
		p.fields[i] = c.field(fd)
		p.names[fd.JSONName()] = int32(i)
		p.names[fd.TextName()] = int32(i)
		if n := int32(fd.Number()); n < denseNumbers {
			for len(p.dense) <= int(n) {
				p.dense = append(p.dense, 0)
			}
			p.dense[n] = int32(i) + 1
		} else {
			p.sparse[n] = int32(i) + 1
		}
	}
	return p
}

func (c *compiler) field(fd protoreflect.FieldDescriptor) field {
	f := c.value(fd)
	f.key = binary.LittleEndian.AppendUint32(nil, uint32(len(fd.JSONName())))
	f.key = append(f.key, fd.JSONName()...)
	f.presence = fd.HasPresence()
	f.oneof = -1
	switch od := fd.ContainingOneof(); {
	case fd.IsMap():
		f.card, f.wtype = mapping, protowire.BytesType
		key, value := c.value(fd.MapKey()), c.value(fd.MapValue())
		f.mapKey, f.mapValue = &key, &value
	case fd.IsList():
		f.card, f.packed = list, fd.IsPacked()
	case od != nil:
		f.oneof, f.unset = int32(od.Index()), unsetAbsent
	case f.kind == protoreflect.MessageKind:
		f.unset = unsetNull
	}
	return f
}

// value is the part of a field that its elements, and a map's keys and
// values, have too.
func (c *compiler) value(fd protoreflect.FieldDescriptor) field {
	f := field{num: fd.Number(), kind: fd.Kind(), wtype: wireType(fd.Kind())}
	switch f.kind {
	case protoreflect.GroupKind:
		c.unsupported = true
	case protoreflect.MessageKind:
		if !fd.IsMap() {
			f.msg = c.message(fd.Message())
		}
	case protoreflect.EnumKind:
		if !c.supported(fd.Enum()) {
			break
		}
		f.enum = map[protoreflect.EnumNumber]string{}
		f.values = map[string]protoreflect.EnumNumber{}
		vs := fd.Enum().Values()
		for i := 0; i < vs.Len(); i++ {
			v := vs.Get(i)
			f.values[string(v.Name())] = v.Number()
			if _, alias := f.enum[v.Number()]; !alias {
				f.enum[v.Number()] = string(v.Name())
			}
		}
	}
	return f
}

func wireType(k protoreflect.Kind) protowire.Type {
	switch k {
	case protoreflect.Fixed32Kind, protoreflect.Sfixed32Kind, protoreflect.FloatKind:
		return protowire.Fixed32Type
	case protoreflect.Fixed64Kind, protoreflect.Sfixed64Kind, protoreflect.DoubleKind:
		return protowire.Fixed64Type
	case protoreflect.StringKind, protoreflect.BytesKind, protoreflect.MessageKind:
		return protowire.BytesType
	}
	return protowire.VarintType
}

// find returns the field with a number, or nil.
func (p *Plan) find(num protowire.Number) (int, *field) {
	var i int32
	if int(num) < len(p.dense) {
		i = p.dense[num]
	} else {
		i = p.sparse[int32(num)]
	}
	if i == 0 {
		return 0, nil
	}
	return int(i - 1), &p.fields[i-1]
}
