package pbwire_test

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/cairophp/cairo/internal/grpcserver"
	"github.com/cairophp/cairo/internal/pbwire"
	"github.com/cairophp/cairo/internal/wire"
)

// What the tests compare with: the message types of testdata/kinds.proto as
// the server compiles them, and PHP's view of JSON text and of
// internal/wire's bytes as one canonical text.

func kinds(tb testing.TB, name protoreflect.Name) (protoreflect.MessageDescriptor, *pbwire.Plan) {
	tb.Helper()
	files, err := grpcserver.CompileProtos([]string{"testdata/kinds.proto"})
	if err != nil {
		tb.Fatal(err)
	}
	d, err := files.FindDescriptorByName(protoreflect.FullName("kinds." + name))
	if err != nil {
		tb.Fatal(err)
	}
	md := d.(protoreflect.MessageDescriptor)
	plan := pbwire.Compile(md)
	if plan == nil {
		tb.Fatalf("no plan for %s", md.FullName())
	}
	return md, plan
}

// phpFromJSON is what json_decode($text, true) gives, written so that two
// equal PHP values have the same text: an array is its keys and values in
// order, a list's keys being 0, 1, 2; an integer literal is an int when it
// fits one and a float otherwise.
func phpFromJSON(tb testing.TB, text []byte) string {
	tb.Helper()
	dec := json.NewDecoder(bytes.NewReader(text))
	dec.UseNumber()
	var b strings.Builder
	var value func()
	value = func() {
		tok, err := dec.Token()
		if err != nil {
			tb.Fatalf("JSON %s: %v", text, err)
		}
		switch v := tok.(type) {
		case nil:
			b.WriteString("null")
		case bool:
			fmt.Fprint(&b, v)
		case json.Number:
			b.WriteString(phpNumber(string(v)))
		case string:
			fmt.Fprintf(&b, "%q", v)
		case json.Delim:
			b.WriteByte('[')
			for i := 0; dec.More(); i++ {
				key := json.Token(strconv.Itoa(i))
				if v == '{' {
					key, _ = dec.Token()
				}
				fmt.Fprintf(&b, "%q=>", key)
				value()
				b.WriteByte(',')
			}
			_, _ = dec.Token()
			b.WriteByte(']')
		}
	}
	value()
	return b.String()
}

// phpNumber follows PHP's JSON scanner (php_json_scanner.re).
func phpNumber(text string) string {
	digits := strings.TrimPrefix(text, "-")
	const min = "9223372036854775808"
	if !strings.ContainsAny(text, ".eE") && (len(digits) < len(min) || len(digits) == len(min) && (digits < min || digits == min && text[0] == '-')) {
		i, _ := strconv.ParseInt(text, 10, 64)
		return "int(" + strconv.FormatInt(i, 10) + ")"
	}
	f, _ := strconv.ParseFloat(text, 64)
	return phpFloat(f)
}

func phpFloat(f float64) string {
	return fmt.Sprintf("float(%v:%x)", f, math.Float64bits(f))
}

// phpFromWire is what internal/core/wire.c decodes, in the same text.
func phpFromWire(tb testing.TB, b []byte) string {
	tb.Helper()
	var out strings.Builder
	u32 := func() int {
		n := binary.LittleEndian.Uint32(b)
		b = b[4:]
		return int(n)
	}
	str := func() string {
		n := u32()
		s := string(b[:n])
		b = b[n:]
		return s
	}
	var value func()
	value = func() {
		tag := b[0]
		b = b[1:]
		switch tag {
		case wire.TagNull:
			out.WriteString("null")
		case wire.TagFalse, wire.TagTrue:
			fmt.Fprint(&out, tag == wire.TagTrue)
		case wire.TagInt:
			out.WriteString("int(" + strconv.FormatInt(int64(binary.LittleEndian.Uint64(b)), 10) + ")")
			b = b[8:]
		case wire.TagFloat:
			out.WriteString(phpFloat(math.Float64frombits(binary.LittleEndian.Uint64(b))))
			b = b[8:]
		case wire.TagString:
			fmt.Fprintf(&out, "%q", str())
		case wire.TagList, wire.TagMap:
			out.WriteByte('[')
			for i, n := 0, u32(); i < n; i++ {
				key := strconv.Itoa(i)
				if tag == wire.TagMap {
					key = str()
				}
				fmt.Fprintf(&out, "%q=>", key)
				value()
				out.WriteByte(',')
			}
			out.WriteByte(']')
		default:
			tb.Fatalf("tag %d in the output", tag)
		}
	}
	value()
	if len(b) != 0 {
		tb.Fatalf("%d bytes after the value", len(b))
	}
	return out.String()
}
