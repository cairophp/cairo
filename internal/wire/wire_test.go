package wire

import (
	"encoding/binary"
	"errors"
	"math"
	"net/http"
	"reflect"
	"testing"
)

// decode mirrors internal/core/wire.c, so the format can be checked without
// PHP: same tags, same bounds checks, same depth limit. JSON values come back
// as their raw text.
func decode(b []byte) (any, error) {
	v, rest, err := decodeValue(b, 0)
	if err == nil && len(rest) != 0 {
		err = errors.New("trailing bytes")
	}
	return v, err
}

var errShort = errors.New("short buffer")

func decodeBytes(b []byte) ([]byte, []byte, error) {
	if len(b) < 4 {
		return nil, nil, errShort
	}
	n := binary.LittleEndian.Uint32(b)
	if uint64(len(b)-4) < uint64(n) {
		return nil, nil, errShort
	}
	return b[4 : 4+n], b[4+n:], nil
}

func decodeValue(b []byte, depth int) (any, []byte, error) {
	if len(b) == 0 {
		return nil, nil, errShort
	}
	tag, b := b[0], b[1:]
	switch tag {
	case TagNull:
		return nil, b, nil
	case TagFalse, TagTrue:
		return tag == TagTrue, b, nil
	case TagInt, TagFloat:
		if len(b) < 8 {
			return nil, nil, errShort
		}
		u := binary.LittleEndian.Uint64(b)
		if tag == TagInt {
			return int64(u), b[8:], nil
		}
		return math.Float64frombits(u), b[8:], nil
	case TagString:
		s, rest, err := decodeBytes(b)
		return string(s), rest, err
	case TagJSON:
		s, rest, err := decodeBytes(b)
		return "json:" + string(s), rest, err
	case TagList, TagMap:
		if depth >= MaxDepth || len(b) < 4 {
			return nil, nil, errors.New("too deep or short")
		}
		n := binary.LittleEndian.Uint32(b)
		b = b[4:]
		if uint64(n) > uint64(len(b)) {
			return nil, nil, errShort
		}
		var list []any
		m := map[string]any{}
		for i := uint32(0); i < n; i++ {
			var key []byte
			var err error
			if tag == TagMap {
				if key, b, err = decodeBytes(b); err != nil {
					return nil, nil, err
				}
			}
			var v any
			if v, b, err = decodeValue(b, depth+1); err != nil {
				return nil, nil, err
			}
			if tag == TagMap {
				m[string(key)] = v
			} else {
				list = append(list, v)
			}
		}
		if tag == TagMap {
			return m, b, nil
		}
		return list, b, nil
	}
	return nil, nil, errors.New("unknown tag")
}

func TestRoundTrip(t *testing.T) {
	in := map[string]any{
		"null": nil, "t": true, "f": false, "int": int64(-42), "float": 1.5,
		"str": "héllo\x00bin", "bytes": []byte{0, 255},
		"list":    []any{int64(1), "two", []any{}},
		"headers": http.Header{"Accept": {"a", "b"}},
		"raw":     []byte(nil),
	}
	got, err := decode(Encode(in))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"null": nil, "t": true, "f": false, "int": int64(-42), "float": 1.5,
		"str": "héllo\x00bin", "bytes": "\x00\xff",
		"list":    []any{int64(1), "two", []any(nil)},
		"headers": map[string]any{"Accept": []any{"a", "b"}},
		"raw":     "",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
}

func TestFallsBackToJSON(t *testing.T) {
	got, err := decode(Encode(struct {
		A int `json:"a"`
	}{7}))
	if err != nil || got != `json:{"a":7}` {
		t.Fatalf("got %#v, %v", got, err)
	}
}

func TestMalformedIsRejected(t *testing.T) {
	deep := []byte{}
	for range MaxDepth + 1 {
		deep = AppendList(deep, 1)
	}
	deep = AppendNull(deep)
	cases := map[string][]byte{
		"empty":         {},
		"unknown tag":   {99},
		"short int":     {TagInt, 1, 2},
		"string length": binary.LittleEndian.AppendUint32([]byte{TagString}, 10),
		"huge count":    binary.LittleEndian.AppendUint32([]byte{TagList}, math.MaxUint32),
		"trailing":      {TagNull, TagNull},
		"too deep":      deep,
	}
	for name, b := range cases {
		if _, err := decode(b); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
}

// The decoder must never read past the buffer or loop on garbage.
func FuzzDecode(f *testing.F) {
	f.Add(Encode(map[string]any{"a": []any{int64(1), "x", nil}}))
	f.Add([]byte{TagMap, 1, 0, 0, 0})
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = decode(b)
	})
}

func BenchmarkEncodeRequest(b *testing.B) {
	h := http.Header{"Host": {"127.0.0.1:8080"}, "User-Agent": {"wrk"}}
	buf := make([]byte, 0, 512)
	for i := 0; i < b.N; i++ {
		buf = buf[:0]
		buf = AppendMap(buf, 4)
		buf = AppendString(AppendKey(buf, "type"), "http")
		buf = AppendInt(AppendKey(buf, "id"), int64(i))
		buf = AppendHeader(AppendKey(buf, "headers"), h)
		buf = AppendBytes(AppendKey(buf, "body"), nil)
	}
}
