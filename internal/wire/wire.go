// Package wire encodes the events Go sends to PHP threads. C decodes them
// straight into PHP arrays (internal/sapi/wire.c), so a request costs no
// JSON text, no json_decode() and no base64 for its body.
//
// A value is a one-byte tag and its payload, little-endian:
//
//	TagNull, TagFalse, TagTrue
//	TagInt    int64
//	TagFloat  float64
//	TagString uint32 length, bytes (binary-safe)
//	TagList   uint32 count, values
//	TagMap    uint32 count, (uint32 key length, key bytes, value) pairs
//	TagJSON   uint32 length, JSON text (decoded by PHP's json extension)
//
// The C decoder checks every length against the buffer and limits nesting
// to MaxDepth.
package wire

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"net/http"
	"sort"
)

// Tags; keep in sync with internal/sapi/wire.h.
const (
	TagNull   = 0
	TagFalse  = 1
	TagTrue   = 2
	TagInt    = 3
	TagFloat  = 4
	TagString = 5
	TagList   = 6
	TagMap    = 7
	TagJSON   = 8
)

// MaxDepth is how deep lists and maps may nest.
const MaxDepth = 64

// Event is what Engine.Dispatch sends to a PHP thread. It appends itself as
// a map, with "id" and, when deadline > 0, "deadline" among its entries.
type Event interface {
	AppendWire(b []byte, id int64, deadline float64) []byte
}

func AppendNull(b []byte) []byte { return append(b, TagNull) }

func AppendBool(b []byte, v bool) []byte {
	if v {
		return append(b, TagTrue)
	}
	return append(b, TagFalse)
}

func AppendInt(b []byte, v int64) []byte {
	return binary.LittleEndian.AppendUint64(append(b, TagInt), uint64(v))
}

func AppendFloat(b []byte, v float64) []byte {
	return binary.LittleEndian.AppendUint64(append(b, TagFloat), math.Float64bits(v))
}

func AppendString(b []byte, s string) []byte {
	b = binary.LittleEndian.AppendUint32(append(b, TagString), uint32(len(s)))
	return append(b, s...)
}

func AppendBytes(b []byte, s []byte) []byte {
	b = binary.LittleEndian.AppendUint32(append(b, TagString), uint32(len(s)))
	return append(b, s...)
}

// AppendJSON appends JSON text that PHP decodes as it would json_decode().
func AppendJSON(b []byte, raw []byte) []byte {
	b = binary.LittleEndian.AppendUint32(append(b, TagJSON), uint32(len(raw)))
	return append(b, raw...)
}

func AppendList(b []byte, n int) []byte {
	return binary.LittleEndian.AppendUint32(append(b, TagList), uint32(n))
}

func AppendMap(b []byte, n int) []byte {
	return binary.LittleEndian.AppendUint32(append(b, TagMap), uint32(n))
}

// AppendKey appends a map key; its value follows.
func AppendKey(b []byte, k string) []byte {
	b = binary.LittleEndian.AppendUint32(b, uint32(len(k)))
	return append(b, k...)
}

// AppendHeader appends HTTP headers as a map of lists, in key order.
func AppendHeader(b []byte, h http.Header) []byte {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	b = AppendMap(b, len(keys))
	for _, k := range keys {
		b = AppendKey(b, k)
		b = AppendList(b, len(h[k]))
		for _, v := range h[k] {
			b = AppendString(b, v)
		}
	}
	return b
}

// AppendValue appends v. Common Go types are encoded directly; anything
// else goes as JSON.
func AppendValue(b []byte, v any) []byte {
	switch v := v.(type) {
	case nil:
		return AppendNull(b)
	case bool:
		return AppendBool(b, v)
	case int:
		return AppendInt(b, int64(v))
	case int32:
		return AppendInt(b, int64(v))
	case int64:
		return AppendInt(b, v)
	case float64:
		return AppendFloat(b, v)
	case string:
		return AppendString(b, v)
	case []byte:
		return AppendBytes(b, v)
	case json.RawMessage:
		if v == nil {
			return AppendNull(b)
		}
		return AppendJSON(b, v)
	case []string:
		b = AppendList(b, len(v))
		for _, s := range v {
			b = AppendString(b, s)
		}
		return b
	case []any:
		b = AppendList(b, len(v))
		for _, x := range v {
			b = AppendValue(b, x)
		}
		return b
	case []map[string]any:
		b = AppendList(b, len(v))
		for _, x := range v {
			b = AppendValue(b, x)
		}
		return b
	case map[string]any:
		b = AppendMap(b, len(v))
		for _, k := range sortedKeys(v) {
			b = AppendValue(AppendKey(b, k), v[k])
		}
		return b
	case map[string]string:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b = AppendMap(b, len(keys))
		for _, k := range keys {
			b = AppendString(AppendKey(b, k), v[k])
		}
		return b
	case http.Header:
		return AppendHeader(b, v)
	case map[string][]string:
		return AppendHeader(b, v)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return AppendNull(b)
	}
	return AppendJSON(b, raw)
}

// Encode returns v encoded on its own.
func Encode(v any) []byte { return AppendValue(nil, v) }

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
