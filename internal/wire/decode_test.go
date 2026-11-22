package wire

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
)

type callArgs struct {
	Target   string              `json:"target"`
	Timeout  float64             `json:"timeout"`
	Retries  int                 `json:"retries"`
	Verify   bool                `json:"verify"`
	Tags     []string            `json:"tags"`
	Headers  map[string][]string `json:"headers"`
	TLS      *tlsArgs            `json:"tls"`
	Message  json.RawMessage     `json:"message"`
	Args     []json.RawMessage   `json:"args"`
	Extra    any                 `json:"extra"`
	Body     []byte              `json:"body"`
	Hidden   string              `json:"-"`
	NoTag    int
	internal int
}

type tlsArgs struct {
	CAFile string `json:"cafile"`
	Verify bool   `json:"verify"`
}

type streamArgs struct {
	callArgs
	ID int64 `json:"id"`
}

// What PHP sends for an operation fills its argument struct as the same
// values in JSON did.
func TestUnmarshalStruct(t *testing.T) {
	data := Encode(map[string]any{
		"target":   "db:5432",
		"timeout":  2, // PHP's int for a float field
		"retries":  3.0,
		"verify":   true,
		"tags":     []string{"a", "b"},
		"headers":  map[string]any{"Accept": []string{"text/plain", "*/*"}},
		"tls":      map[string]any{"cafile": "/ca.pem", "verify": true},
		"message":  map[string]any{"name": "Cairo", "n": 1.0, "list": []any{}},
		"args":     []any{int64(7), "x\"y", nil, 1.5},
		"extra":    []any{int64(1), "two", map[string]any{"three": 3.5}},
		"body":     "\xff\x00raw",
		"Hidden":   "kept out",
		"internal": 5, // not exported: skipped
		"notag":    4, // matched without regard to case
		"unknown":  map[string]any{"skipped": []any{int64(1), "x"}},
		"id":       int64(9),
	})
	var got streamArgs
	if err := Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	want := streamArgs{ID: 9, callArgs: callArgs{
		Target: "db:5432", Timeout: 2, Retries: 3, Verify: true,
		Tags:    []string{"a", "b"},
		Headers: map[string][]string{"Accept": {"text/plain", "*/*"}},
		TLS:     &tlsArgs{CAFile: "/ca.pem", Verify: true},
		Message: json.RawMessage(`{"list":[],"n":1.0,"name":"Cairo"}`),
		Args:    []json.RawMessage{json.RawMessage(`7`), json.RawMessage(`"x\"y"`), json.RawMessage(`null`), json.RawMessage(`1.5`)},
		Extra:   []any{int64(1), "two", map[string]any{"three": 3.5}},
		Body:    []byte("\xff\x00raw"),
		NoTag:   4,
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

// PHP has one empty array for a list and a map, and null for what is absent.
func TestUnmarshalEmptyAndNull(t *testing.T) {
	got := callArgs{Target: "old", TLS: &tlsArgs{}, Headers: map[string][]string{"a": nil}}
	data := Encode(map[string]any{"target": nil, "tls": nil, "headers": []any{}, "tags": []any{}})
	if err := Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Target != "" || got.TLS != nil || len(got.Headers) != 0 || got.Tags == nil || len(got.Tags) != 0 {
		t.Errorf("after null and []: %+v", got)
	}
	var none callArgs
	if err := Unmarshal(Encode([]any{}), &none); err != nil {
		t.Errorf("[] for a struct: %v", err)
	}
}

// An object that PHP's json extension encoded arrives as JSON text.
func TestUnmarshalJSONValue(t *testing.T) {
	b := AppendMap(nil, 2)
	b = AppendJSON(AppendKey(b, "tls"), []byte(`{"cafile":"/ca.pem"}`))
	b = AppendJSON(AppendKey(b, "message"), []byte(`{"a":[1,2]}`))
	var got callArgs
	if err := Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.TLS == nil || got.TLS.CAFile != "/ca.pem" || string(got.Message) != `{"a":[1,2]}` {
		t.Errorf("got %+v, message %s", got, got.Message)
	}
}

func TestUnmarshalRefuses(t *testing.T) {
	deep := []byte{}
	for i := 0; i <= MaxDepth; i++ {
		deep = AppendList(deep, 1)
	}
	deep = AppendNull(deep)
	for _, tc := range []struct {
		name string
		data []byte
		into any
		want string
	}{
		{"a string for a number", Encode(map[string]any{"retries": "3"}), new(callArgs), "does not fit"},
		{"a fraction for an int", Encode(map[string]any{"retries": 2.5}), new(callArgs), "does not fit"},
		{"an int too large", Encode(int64(300)), new(int8), "does not fit"},
		{"a negative for unsigned", Encode(int64(-1)), new(uint), "does not fit"},
		{"a list for a struct", Encode([]any{int64(1)}), new(callArgs), "does not fit"},
		{"a map for a list", Encode(map[string]any{"tags": map[string]any{"a": "b"}}), new(callArgs), "does not fit"},
		{"text that is not UTF-8 for JSON", Encode(map[string]any{"message": "\xff"}), new(callArgs), "UTF-8"},
		{"NaN for JSON", Encode(map[string]any{"message": math.NaN()}), new(callArgs), "NaN"},
		{"nesting too deep", deep, new(any), "too deep"},
		{"a count beyond the buffer", AppendList(nil, 1<<30), new([]int), "cut short"},
		{"bytes after the value", append(Encode(int64(1)), 0), new(int), "bytes left"},
		{"an unknown tag", []byte{99}, new(any), "unknown tag"},
		{"no pointer", Encode(int64(1)), 0, "pointer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Unmarshal(tc.data, tc.into)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %v, want one about %q", err, tc.want)
			}
		})
	}
}

// Cut anywhere, a value is an error and never a read past the buffer.
func TestUnmarshalCutShort(t *testing.T) {
	data := Encode(map[string]any{
		"target": "db", "timeout": 1.5, "tags": []string{"a"}, "message": map[string]any{"k": []any{int64(1), "s"}},
		"extra": map[string]any{"x": []any{true, nil}}, "unknown": []any{"skipped", 2.5},
	})
	for n := 0; n < len(data); n++ {
		var got callArgs
		if err := Unmarshal(data[:n], &got); err == nil {
			t.Fatalf("the first %d of %d bytes decoded without an error", n, len(data))
		}
	}
}

// The same arguments as JSON text and in the binary form.
func BenchmarkUnmarshal(b *testing.B) {
	args := map[string]any{
		"target": "db:5432", "timeout": 2.5, "retries": 3, "verify": true, "tags": []string{"a", "b"},
		"headers": map[string]any{"Accept": []string{"text/plain"}},
		"tls":     map[string]any{"cafile": "/ca.pem", "verify": true},
	}
	text, err := json.Marshal(args)
	if err != nil {
		b.Fatal(err)
	}
	data := Encode(args)
	b.Run("json", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			var got callArgs
			if err := json.Unmarshal(text, &got); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("wire", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			var got callArgs
			if err := Unmarshal(data, &got); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// A Raw field gets its value's own bytes, whatever the value, and they go
// back out unchanged.
func TestUnmarshalRaw(t *testing.T) {
	message := map[string]any{"list": []any{int64(1), "two"}, "n": 1.5}
	var got struct {
		ID      int64 `json:"id"`
		Message Raw   `json:"message"`
		Missing Raw   `json:"missing"`
	}
	if err := Unmarshal(Encode(map[string]any{"id": int64(7), "message": message}), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != 7 || string(got.Message) != string(Encode(message)) || got.Missing != nil {
		t.Fatalf("got %+v", got)
	}
	if out := Encode(map[string]any{"message": got.Message, "missing": got.Missing}); string(out) != string(Encode(map[string]any{"message": message, "missing": nil})) {
		t.Fatalf("appended as % x", out)
	}
	cut := Encode(map[string]any{"message": message})
	if err := Unmarshal(cut[:len(cut)-3], &got); err == nil {
		t.Fatal("a value cut short filled a Raw")
	}
}
