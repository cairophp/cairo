package wire

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
)

// Unmarshal decodes one value, what PHP sent as the arguments of a Go
// operation (internal/core/wire.c encodes them), into the value v points to.
//
// It reads like encoding/json, so that an operation's argument struct keeps
// its `json` tags: struct fields are matched by tag, then by name; unknown
// keys are skipped; null leaves a zero value. Three things differ, because
// the values come from PHP and not from JSON text: a string is binary-safe
// and fills a []byte as it is; a number fills any numeric field that holds
// it exactly; and an empty list also fills a struct or a map, since PHP has
// one empty array for both. A json.RawMessage field gets its value as JSON
// text, which is how an operation passes a part of its arguments on.
//
// Every length is checked against the buffer and nesting is limited to
// MaxDepth, as in the C decoder.
func Unmarshal(data []byte, v any) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return errors.New("wire: Unmarshal needs a pointer")
	}
	r := reader{b: data}
	if err := r.value(rv.Elem(), 0); err != nil {
		return err
	}
	if r.i != len(r.b) {
		return errors.New("wire: bytes left after the value")
	}
	return nil
}

var errCut = errors.New("wire: value cut short")

type reader struct {
	b []byte
	i int
}

func (r *reader) tag() (byte, error) {
	if r.i >= len(r.b) {
		return 0, errCut
	}
	r.i++
	return r.b[r.i-1], nil
}

func (r *reader) u32() (int, error) {
	if len(r.b)-r.i < 4 {
		return 0, errCut
	}
	r.i += 4
	return int(binary.LittleEndian.Uint32(r.b[r.i-4:])), nil
}

func (r *reader) u64() (uint64, error) {
	if len(r.b)-r.i < 8 {
		return 0, errCut
	}
	r.i += 8
	return binary.LittleEndian.Uint64(r.b[r.i-8:]), nil
}

// bytes reads a length-prefixed run of bytes, a slice of the buffer.
func (r *reader) bytes() ([]byte, error) {
	n, err := r.u32()
	if err != nil || len(r.b)-r.i < n {
		return nil, errCut
	}
	r.i += n
	return r.b[r.i-n : r.i], nil
}

// count reads how many entries a list or a map has. Each takes at least one
// byte, so a count beyond what is left is malformed.
func (r *reader) count(depth int) (int, error) {
	if depth >= MaxDepth {
		return 0, errors.New("wire: nested too deep")
	}
	n, err := r.u32()
	if err != nil || n > len(r.b)-r.i {
		return 0, errCut
	}
	return n, nil
}

var rawMessage = reflect.TypeOf(json.RawMessage(nil))

func (r *reader) value(v reflect.Value, depth int) error {
	if v.Type() == rawMessage {
		raw, err := r.appendJSON(nil, depth)
		if err != nil {
			return err
		}
		v.SetBytes(raw)
		return nil
	}
	if v.Kind() == reflect.Pointer {
		if r.i < len(r.b) && r.b[r.i] == TagNull {
			r.i++
			v.SetZero()
			return nil
		}
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		return r.value(v.Elem(), depth)
	}
	if v.Kind() == reflect.Interface && v.NumMethod() == 0 {
		x, err := r.any(depth)
		if err == nil && x != nil {
			v.Set(reflect.ValueOf(x))
		} else {
			v.SetZero()
		}
		return err
	}
	tag, err := r.tag()
	if err != nil {
		return err
	}
	switch tag {
	case TagNull:
		v.SetZero()
		return nil
	case TagFalse, TagTrue:
		if v.Kind() != reflect.Bool {
			return mismatch("a bool", v)
		}
		v.SetBool(tag == TagTrue)
		return nil
	case TagInt:
		u, err := r.u64()
		if err != nil {
			return err
		}
		return setInt(v, int64(u))
	case TagFloat:
		u, err := r.u64()
		if err != nil {
			return err
		}
		return setFloat(v, math.Float64frombits(u))
	case TagString:
		s, err := r.bytes()
		if err != nil {
			return err
		}
		switch {
		case v.Kind() == reflect.String:
			v.SetString(string(s))
		case v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.Uint8:
			v.SetBytes(append([]byte(nil), s...))
		default:
			return mismatch("a string", v)
		}
		return nil
	case TagList:
		return r.list(v, depth)
	case TagMap:
		return r.mapping(v, depth)
	case TagJSON:
		// An object that PHP's json extension encoded (wire.c).
		s, err := r.bytes()
		if err != nil {
			return err
		}
		return json.Unmarshal(s, v.Addr().Interface())
	}
	return fmt.Errorf("wire: unknown tag %d", tag)
}

func mismatch(what string, v reflect.Value) error {
	return fmt.Errorf("wire: %s does not fit a %s", what, v.Type())
}

func setInt(v reflect.Value, i int64) error {
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if v.OverflowInt(i) {
			return mismatch(fmt.Sprintf("the integer %d", i), v)
		}
		v.SetInt(i)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if i < 0 || v.OverflowUint(uint64(i)) {
			return mismatch(fmt.Sprintf("the integer %d", i), v)
		}
		v.SetUint(uint64(i))
	case reflect.Float32, reflect.Float64:
		v.SetFloat(float64(i))
	default:
		return mismatch("an integer", v)
	}
	return nil
}

func setFloat(v reflect.Value, f float64) error {
	switch v.Kind() {
	case reflect.Float32, reflect.Float64:
		v.SetFloat(f)
		return nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		// PHP's 2.0 for a count: exact, so it fits.
		if f == math.Trunc(f) && math.Abs(f) < 1<<53 {
			return setInt(v, int64(f))
		}
	}
	return mismatch("a float", v)
}

func (r *reader) list(v reflect.Value, depth int) error {
	n, err := r.count(depth)
	if err != nil {
		return err
	}
	switch v.Kind() {
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), n, n))
		for i := 0; i < n; i++ {
			if err := r.value(v.Index(i), depth+1); err != nil {
				return err
			}
		}
		return nil
	case reflect.Struct, reflect.Map:
		if n == 0 {
			v.SetZero() // PHP's [] is its empty map too
			return nil
		}
	}
	return mismatch("a list", v)
}

func (r *reader) mapping(v reflect.Value, depth int) error {
	n, err := r.count(depth)
	if err != nil {
		return err
	}
	switch v.Kind() {
	case reflect.Struct:
		fields := fieldsOf(v.Type())
		for i := 0; i < n; i++ {
			key, err := r.bytes()
			if err != nil {
				return err
			}
			if f, ok := fields.find(key); ok {
				err = r.value(v.FieldByIndex(f), depth+1)
			} else {
				err = r.skip(depth + 1)
			}
			if err != nil {
				return err
			}
		}
		return nil
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			break
		}
		v.Set(reflect.MakeMapWithSize(v.Type(), n))
		elem := reflect.New(v.Type().Elem()).Elem()
		for i := 0; i < n; i++ {
			key, err := r.bytes()
			if err != nil {
				return err
			}
			elem.SetZero()
			if err := r.value(elem, depth+1); err != nil {
				return err
			}
			v.SetMapIndex(reflect.ValueOf(string(key)).Convert(v.Type().Key()), elem)
		}
		return nil
	}
	return mismatch("a map", v)
}

// structFields is where each key of a struct type goes: the index path of
// the field with that `json` name, the fields of an embedded struct included.
type structFields map[string][]int

var fieldCache sync.Map // reflect.Type -> structFields

func fieldsOf(t reflect.Type) structFields {
	if f, ok := fieldCache.Load(t); ok {
		return f.(structFields)
	}
	fields := structFields{}
	for _, f := range reflect.VisibleFields(t) {
		if !f.IsExported() || f.Anonymous {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		if _, taken := fields[name]; !taken {
			fields[name] = f.Index
		}
	}
	fieldCache.Store(t, fields)
	return fields
}

// find looks a key up as it is and then, as encoding/json does, without
// regard to case.
func (f structFields) find(key []byte) ([]int, bool) {
	if i, ok := f[string(key)]; ok {
		return i, true
	}
	for name, i := range f {
		if strings.EqualFold(name, string(key)) {
			return i, true
		}
	}
	return nil, false
}
