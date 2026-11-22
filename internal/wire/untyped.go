package wire

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"unicode/utf8"
)

// What Unmarshal does with a value that no typed field takes: an
// interface{} gets Go's plain types, a json.RawMessage gets JSON text, and
// the value of an unknown key is passed over.

// any decodes a value for an interface{} field: nil, bool, int64, float64,
// string, []any or map[string]any.
func (r *reader) any(depth int) (any, error) {
	tag, err := r.tag()
	if err != nil {
		return nil, err
	}
	switch tag {
	case TagNull:
		return nil, nil
	case TagFalse, TagTrue:
		return tag == TagTrue, nil
	case TagInt:
		u, err := r.u64()
		return int64(u), err
	case TagFloat:
		u, err := r.u64()
		return math.Float64frombits(u), err
	case TagString:
		s, err := r.bytes()
		return string(s), err
	case TagJSON:
		s, err := r.bytes()
		if err != nil {
			return nil, err
		}
		var x any
		err = json.Unmarshal(s, &x)
		return x, err
	case TagList:
		n, err := r.count(depth)
		if err != nil {
			return nil, err
		}
		list := make([]any, n)
		for i := range list {
			if list[i], err = r.any(depth + 1); err != nil {
				return nil, err
			}
		}
		return list, nil
	case TagMap:
		n, err := r.count(depth)
		if err != nil {
			return nil, err
		}
		m := make(map[string]any, n)
		for i := 0; i < n; i++ {
			key, err := r.bytes()
			if err != nil {
				return nil, err
			}
			if m[string(key)], err = r.any(depth + 1); err != nil {
				return nil, err
			}
		}
		return m, nil
	}
	return nil, fmt.Errorf("wire: unknown tag %d", tag)
}

// skip passes over one value, of a key no field takes.
func (r *reader) skip(depth int) error {
	tag, err := r.tag()
	if err != nil {
		return err
	}
	switch tag {
	case TagNull, TagFalse, TagTrue:
		return nil
	case TagInt, TagFloat:
		_, err = r.u64()
		return err
	case TagString, TagJSON:
		_, err = r.bytes()
		return err
	case TagList, TagMap:
		n, err := r.count(depth)
		for i := 0; i < n && err == nil; i++ {
			if tag == TagMap {
				_, err = r.bytes()
			}
			if err == nil {
				err = r.skip(depth + 1)
			}
		}
		return err
	}
	return fmt.Errorf("wire: unknown tag %d", tag)
}

// appendJSON appends the next value to b as JSON text, the text that PHP's
// json_encode() gives: a float keeps its fraction ("1.0"), and a string that
// is not UTF-8 is an error.
func (r *reader) appendJSON(b []byte, depth int) ([]byte, error) {
	tag, err := r.tag()
	if err != nil {
		return nil, err
	}
	switch tag {
	case TagNull:
		return append(b, "null"...), nil
	case TagFalse:
		return append(b, "false"...), nil
	case TagTrue:
		return append(b, "true"...), nil
	case TagInt:
		u, err := r.u64()
		return strconv.AppendInt(b, int64(u), 10), err
	case TagFloat:
		u, err := r.u64()
		if err != nil {
			return nil, err
		}
		return AppendJSONFloat(b, math.Float64frombits(u))
	case TagString, TagJSON:
		s, err := r.bytes()
		if err != nil {
			return nil, err
		}
		if tag == TagJSON {
			return append(b, s...), nil
		}
		return appendJSONString(b, s)
	case TagList, TagMap:
		n, err := r.count(depth)
		if err != nil {
			return nil, err
		}
		b = append(b, "[{"[tag-TagList])
		for i := 0; i < n; i++ {
			if i > 0 {
				b = append(b, ',')
			}
			if tag == TagMap {
				key, err := r.bytes()
				if err != nil {
					return nil, err
				}
				if b, err = appendJSONString(b, key); err != nil {
					return nil, err
				}
				b = append(b, ':')
			}
			if b, err = r.appendJSON(b, depth+1); err != nil {
				return nil, err
			}
		}
		return append(b, "]}"[tag-TagList]), nil
	}
	return nil, fmt.Errorf("wire: unknown tag %d", tag)
}

// AppendJSONFloat appends a float as the JSON text that Unmarshal gives a
// json.RawMessage for it.
func AppendJSONFloat(b []byte, f float64) ([]byte, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, errors.New("wire: NaN and Inf have no JSON form")
	}
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return strconv.AppendFloat(b, f, 'f', 1, 64), nil
	}
	return strconv.AppendFloat(b, f, 'g', -1, 64), nil
}

func appendJSONString(b, s []byte) ([]byte, error) {
	if !utf8.Valid(s) {
		return nil, errors.New("wire: a string is not UTF-8, which JSON needs")
	}
	b = append(b, '"')
	for _, c := range s {
		switch {
		case c == '"' || c == '\\':
			b = append(b, '\\', c)
		case c < 0x20:
			b = append(b, '\\', 'u', '0', '0', "0123456789abcdef"[c>>4], "0123456789abcdef"[c&15])
		default:
			b = append(b, c)
		}
	}
	return append(b, '"'), nil
}
