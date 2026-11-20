package httpserver

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/cairophp/cairo/internal/wire"
)

func TestDecodeAnswer(t *testing.T) {
	b := wire.AppendInt(nil, 201)
	b = wire.AppendHeader(b, http.Header{"X-A": {"1", "2"}, "Content-Type": {"text/plain"}})
	status, h, err := decodeAnswer(b)
	if err != nil || status != 201 || !reflect.DeepEqual(h, http.Header{"X-A": {"1", "2"}, "Content-Type": {"text/plain"}}) {
		t.Fatalf("got %d %v %v", status, h, err)
	}
	for i := range b {
		if _, _, err := decodeAnswer(b[:i]); err == nil {
			t.Fatalf("decoded a buffer cut at %d of %d", i, len(b))
		}
	}
}

// The decoder must never read past the buffer or loop on garbage.
func FuzzDecodeAnswer(f *testing.F) {
	f.Add(wire.AppendHeader(wire.AppendInt(nil, 200), http.Header{"A": {"b"}}))
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _, _ = decodeAnswer(b)
	})
}
