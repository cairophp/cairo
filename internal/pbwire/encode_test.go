package pbwire_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"math"
	"math/rand"
	"strconv"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/cairophp/cairo/internal/pbwire"
	"github.com/cairophp/cairo/internal/pbwire/pbwiretest"
	"github.com/cairophp/cairo/internal/wire"
)

// checkProto holds AppendProto to its contract for one value from PHP: what
// protojson refuses is not converted, and what is converted is the message
// that protojson reads from the value's JSON text. It returns whether the
// value was converted, and whether protojson reads it.
func checkProto(tb testing.TB, md protoreflect.MessageDescriptor, plan *pbwire.Plan, value []byte) (converted, readable bool) {
	tb.Helper()
	prefix := []byte("kept")
	got, ok := plan.AppendProto(prefix, value)
	if string(got[:len(prefix)]) != "kept" || !ok && len(got) != len(prefix) {
		tb.Fatalf("% x: what was there before is not kept", value)
	}
	var text json.RawMessage
	want := dynamicpb.NewMessage(md)
	err := wire.Unmarshal(value, &text)
	if err == nil {
		err = protojson.Unmarshal(text, want)
	}
	if err != nil {
		if ok {
			tb.Fatalf("% x (%s): converted, but today's path says %v", value, text, err)
		}
		return false, false
	}
	if !ok {
		return false, true
	}
	have := dynamicpb.NewMessage(md)
	if err := proto.Unmarshal(got[len(prefix):], have); err != nil {
		tb.Fatalf("%s: the bytes are no message: %v", text, err)
	}
	if !proto.Equal(want, have) {
		tb.Fatalf("%s\nprotojson: %s\npbwire:    %s", text, protojson.Format(want), protojson.Format(have))
	}
	return true, true
}

// What AppendWire gives PHP comes back as the same message.
func TestAppendProtoReadsAppendWire(t *testing.T) {
	md, plan := kinds(t, "All")
	r := rand.New(rand.NewSource(4))
	for i := 0; i < 1000; i++ {
		m := pbwiretest.Message(r, md, 0)
		data, _ := proto.Marshal(m)
		value, ok := plan.AppendWire(nil, data)
		if !ok {
			t.Fatalf("% x: not converted", data)
		}
		text, _ := protojson.Marshal(m)
		// NaN and the infinities are strings in JSON, which is not a form
		// this package reads a float from.
		plain := !bytes.Contains(text, []byte("NaN")) && !bytes.Contains(text, []byte("Infinity"))
		if converted, _ := checkProto(t, md, plan, value); !converted && plain {
			t.Fatalf("%s: left to protojson", text)
		}
	}
}

// Values as a handler may return them. The plain ones are in the forms this
// package takes, so each that protojson reads is converted; the odd ones are
// what protojson reads in another form, or refuses.
func TestAppendProtoRandomValues(t *testing.T) {
	md, plan := kinds(t, "All")
	for name, odd := range map[string]int{"plain": 0, "odd": 40} {
		g := phpValues{rand.New(rand.NewSource(5)), odd}
		count := 0
		for i := 0; i < 5000; i++ {
			value := g.message(nil, md, 0)
			converted, readable := checkProto(t, md, plan, value)
			if converted {
				count++
			} else if readable && odd == 0 {
				var text json.RawMessage
				_ = wire.Unmarshal(value, &text)
				t.Fatalf("%s: left to protojson, which reads it", text)
			}
		}
		if count < 1000 {
			t.Errorf("%s: only %d of 5000 values were converted", name, count)
		}
	}
}

// phpValues makes values; one in odd is not what the field calls for.
type phpValues struct {
	r   *rand.Rand
	odd int
}

func (g phpValues) oddly() bool { return g.odd > 0 && g.r.Intn(g.odd) == 0 }

func (g phpValues) message(b []byte, md protoreflect.MessageDescriptor, depth int) []byte {
	fds := md.Fields()
	var keys []string
	var fields []protoreflect.FieldDescriptor
	for _, i := range g.r.Perm(fds.Len()) {
		fd := fds.Get(i)
		if g.r.Intn(4) != 0 || fd.ContainingOneof() != nil && !fd.HasOptionalKeyword() && !g.oddly() {
			continue
		}
		fields, keys = append(fields, fd), append(keys, fd.JSONName())
		if g.r.Intn(12) == 0 {
			keys[len(keys)-1] = fd.TextName()
		}
		if g.oddly() { // the same field under its other name
			fields, keys = append(fields, fd), append(keys, fd.TextName())
		}
	}
	if g.oddly() {
		fields, keys = append(fields, fds.Get(0)), append(keys, "nobody")
	}
	if one := md.Oneofs().ByName("choice"); one != nil && g.r.Intn(2) == 0 {
		fd := one.Fields().Get(g.r.Intn(one.Fields().Len()))
		fields, keys = append(fields, fd), append(keys, fd.JSONName())
	}
	b = wire.AppendMap(b, len(fields))
	for i, fd := range fields {
		b = wire.AppendKey(b, keys[i])
		switch {
		case g.oddly():
			b = g.anything(b)
		case fd.IsList():
			n := g.r.Intn(4)
			b = wire.AppendList(b, n)
			for ; n > 0; n-- {
				b = g.value(b, fd, depth)
			}
		case fd.IsMap():
			seen := map[string]bool{}
			var entries []byte
			for n := g.r.Intn(4); n > 0; n-- {
				if key := g.key(fd.MapKey()); !seen[key] || g.oddly() {
					seen[key] = true
					entries = g.value(wire.AppendKey(entries, key), fd.MapValue(), depth)
				}
			}
			b = append(wire.AppendMap(b, len(seen)), entries...)
		default:
			b = g.value(b, fd, depth)
		}
	}
	return b
}

// anything is a value of whatever type, wanted or not.
func (g phpValues) anything(b []byte) []byte {
	switch g.r.Intn(8) {
	case 0:
		return wire.AppendNull(b)
	case 1:
		return wire.AppendBool(b, g.r.Intn(2) == 0)
	case 2:
		return wire.AppendInt(b, pbwiretest.Int(g.r))
	case 3:
		return wire.AppendFloat(b, pbwiretest.Float(g.r))
	case 4:
		return wire.AppendString(b, pbwiretest.String(g.r))
	case 5:
		return wire.AppendList(b, 0)
	case 6:
		return wire.AppendMap(b, 0)
	}
	return wire.AppendJSON(b, []byte(`{"name":"from JsonSerializable"}`))
}

func (g phpValues) key(fd protoreflect.FieldDescriptor) string {
	switch fd.Kind() {
	case protoreflect.StringKind:
		return pbwiretest.String(g.r)
	case protoreflect.BoolKind:
		return strconv.FormatBool(g.r.Intn(2) == 0)
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return strconv.FormatUint(uint64(pbwiretest.Int(g.r)), 10)
	case protoreflect.Int64Kind:
		return strconv.FormatInt(pbwiretest.Int(g.r), 10)
	}
	if g.oddly() {
		return []string{"05", "+1", "-0", "x", "", "1.0", "\xff", "2147483648"}[g.r.Intn(8)]
	}
	return strconv.FormatInt(int64(int32(pbwiretest.Int(g.r))), 10)
}

var (
	oddNumbers = []string{"-0", "01", " 1", "1 ", "1.0", "1e3", "+1", "", "0x10", "1,2", "9223372036854775808", "18446744073709551616", "NaN", "Infinity", "-Infinity", "1.5", "7"}
	oddBytes   = []string{"aGk", "a-_=", "aGk=\n", "!!!!", "aGk=aGk=", "a", "YQ", "-_-_"}
)

func (g phpValues) value(b []byte, fd protoreflect.FieldDescriptor, depth int) []byte {
	r, odd := g.r, g.oddly()
	switch fd.Kind() {
	case protoreflect.BoolKind:
		return wire.AppendBool(b, r.Intn(2) == 0)
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return g.number(b, int64(int32(pbwiretest.Int(r))), odd)
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return g.number(b, int64(uint32(pbwiretest.Int(r))), odd)
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		if v := pbwiretest.Int(r); !odd && r.Intn(2) == 0 {
			return wire.AppendString(b, strconv.FormatInt(v, 10))
		}
		return g.number(b, pbwiretest.Int(r), odd)
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		if v := uint64(pbwiretest.Int(r)); !odd && (r.Intn(2) == 0 || v > math.MaxInt64) {
			return wire.AppendString(b, strconv.FormatUint(v, 10))
		}
		return g.number(b, pbwiretest.Int(r)&math.MaxInt64, odd)
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		switch {
		case odd:
			return wire.AppendString(b, oddNumbers[r.Intn(len(oddNumbers))])
		case r.Intn(4) == 0:
			return wire.AppendInt(b, pbwiretest.Int(r))
		}
		return wire.AppendFloat(b, pbwiretest.Float(r))
	case protoreflect.StringKind:
		if odd {
			return wire.AppendString(b, "\xc3\x28 is not UTF-8")
		}
		return wire.AppendString(b, pbwiretest.String(r))
	case protoreflect.BytesKind:
		if odd {
			return wire.AppendString(b, oddBytes[r.Intn(len(oddBytes))])
		}
		return wire.AppendString(b, base64.StdEncoding.EncodeToString(pbwiretest.Bytes(r)))
	case protoreflect.EnumKind:
		switch {
		case odd:
			return wire.AppendString(b, []string{"PURPLE", "1", "red", ""}[r.Intn(4)])
		case r.Intn(2) == 0:
			return wire.AppendInt(b, []int64{0, 1, 2, -1, 77, math.MaxInt32, math.MinInt32}[r.Intn(7)])
		}
		return wire.AppendString(b, []string{"COLOR_UNSET", "RED", "CRIMSON", "GREEN", "BELOW"}[r.Intn(5)])
	}
	if depth >= 3 {
		return wire.AppendMap(b, 0)
	}
	return g.message(b, fd.Message(), depth+1)
}

// number is an integer as an int or, oddly, as something else that may or
// may not be read as one.
func (g phpValues) number(b []byte, v int64, odd bool) []byte {
	if !odd {
		return wire.AppendInt(b, v)
	}
	switch g.r.Intn(4) {
	case 0:
		return wire.AppendFloat(b, float64(int32(v)))
	case 1:
		return wire.AppendString(b, strconv.FormatInt(v, 10))
	case 2:
		return wire.AppendString(b, oddNumbers[g.r.Intn(len(oddNumbers))])
	}
	return wire.AppendInt(b, pbwiretest.Int(g.r))
}

func TestAppendProtoForms(t *testing.T) {
	md, plan := kinds(t, "All")
	type m = map[string]any
	deep := m{}
	for i, inner := 0, deep; i < 70; i++ {
		next := m{}
		inner["fSelf"] = next
		inner = next
	}
	cases := []struct {
		value     any
		converted bool
	}{
		{m{}, true},
		{[]any{}, false}, // PHP's [] where a message is expected
		{m{"fInt32": 5, "fString": "héllo", "fBool": true}, true},
		{m{"f_int32": 5}, true},
		{m{"fInt32": 5, "f_int32": 5}, false},
		{m{"nobody": 1}, false},
		{m{"fInt32": "5"}, false}, // protojson reads it; this package leaves it
		{m{"fInt32": 5.0}, false},
		{m{"fInt32": math.MaxInt32 + 1}, false},
		{m{"fUint32": -1}, false},
		{m{"fInt64": "5", "fUint64": "18446744073709551615", "fSint64": "-9223372036854775808"}, true},
		{m{"fInt64": 5, "fSfixed64": math.MinInt64, "fFixed64": math.MaxInt64}, true},
		{m{"fInt64": "05"}, false},
		{m{"fInt64": "9223372036854775808"}, false},
		{m{"fUint64": "18446744073709551616"}, false},
		{m{"fUint64": -1}, false},
		{m{"fFloat": 0.1, "fDouble": 0.1}, true},
		{m{"fFloat": 5, "fDouble": math.MaxInt64}, true},
		{m{"fFloat": 1e39}, false},
		{m{"fDouble": math.NaN()}, false},
		{m{"fDouble": "NaN"}, false},
		{m{"fDouble": math.Copysign(0, -1)}, true},
		{m{"fBytes": "aGk="}, true},
		{m{"fBytes": "aGk"}, false},
		{m{"fBytes": "a-k="}, false},
		{m{"fBytes": "!!!!"}, false},
		{m{"fString": "\xff"}, false},
		{m{"fEnum": "RED"}, true},
		{m{"fEnum": "CRIMSON"}, true},
		{m{"fEnum": 77}, true},
		{m{"fEnum": "PURPLE"}, false},
		{m{"fMsg": m{}, "fSelf": m{"fMsg": m{"name": "x"}}}, true},
		{m{"fMsg": nil, "rInt32": nil, "mString": nil}, true},
		{m{"fMsg": []any{}}, false},
		{m{"rInt32": []any{1, 2, 3}, "rUnpacked": []any{1, 2}, "rString": []any{"a", ""}, "rMsg": []any{m{}, m{"n": 1}}}, true},
		{m{"rInt32": []any{}, "rMsg": []any{}}, true},
		{m{"rInt32": []any{nil}}, false},
		{m{"rInt32": m{"1": 1}}, false},
		{m{"rMsg": []any{nil}}, false},
		{m{"mInt32": m{"5": "a", "-7": "b"}, "mBool": m{"true": 1.5}, "mInt64": m{"9": m{"name": "x"}}, "mString": m{"": 0}}, true},
		{m{"mInt32": m{"05": "a"}}, false},
		{m{"mBool": m{"TRUE": 1.5}}, false},
		{m{"mString": []any{}}, false},
		{m{"mString": m{"a": nil}}, false},
		{m{"oInt32": 0, "oString": "", "oMsg": m{}}, true},
		{m{"cInt32": 0}, true},
		{m{"cInt32": 1, "cString": "x"}, false},
		{m{"cInt32": nil, "cString": "x"}, true},
		{m{"otherName": "x", "farNumber": 9}, true},
		{m{"renamed": "x"}, true},
		{deep, false}, // deeper than internal/wire goes
	}
	for _, c := range cases {
		value := wire.Encode(c.value)
		if got, _ := checkProto(t, md, plan, value); got != c.converted {
			t.Errorf("%v: converted is %v, want %v", c.value, got, c.converted)
		}
	}
}

func FuzzAppendProto(f *testing.F) {
	md, plan := kinds(f, "All")
	r := rand.New(rand.NewSource(6))
	for i := 0; i < 20; i++ {
		f.Add(phpValues{r, 40}.message(nil, md, 0))
	}
	f.Fuzz(func(t *testing.T, value []byte) {
		checkProto(t, md, plan, value)
	})
}

func BenchmarkAppendProto(b *testing.B) {
	_, plan := kinds(b, "Leaf")
	value := wire.Encode(map[string]any{"name": "Hello, world", "n": 42})
	var buf []byte
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		buf, _ = plan.AppendProto(buf[:0], value)
	}
}
