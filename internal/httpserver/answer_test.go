package httpserver

import (
	"net/http"
	"net/http/httptest"
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

// PHP spells header names as the app did. Go's are canonical, and the cache
// looks for Set-Cookie by that name.
func TestDecodeAnswerMakesNamesCanonical(t *testing.T) {
	b := wire.AppendInt(nil, 200)
	b = wire.AppendMap(b, 3)
	for _, kv := range [][2]string{{"set-cookie", "a=1"}, {"x-b", "2"}, {"Set-Cookie", "c=3"}} {
		b = wire.AppendKey(b, kv[0])
		b = wire.AppendList(b, 1)
		b = wire.AppendString(b, kv[1])
	}
	_, h, err := decodeAnswer(b)
	want := http.Header{"Set-Cookie": {"a=1", "c=3"}, "X-B": {"2"}}
	if err != nil || !reflect.DeepEqual(h, want) {
		t.Fatalf("got %v %v, want %v", h, err, want)
	}
}

// A response's header may be the cache's: writing it to a client whose
// header already has one of its names must leave it as it was.
func TestWriteLeavesTheResponseAlone(t *testing.T) {
	b := wire.AppendHeader(wire.AppendInt(nil, 200), http.Header{"vary": {"Accept"}, "X-A": {"1"}, "X-B": {"2"}})
	_, h, err := decodeAnswer(b)
	if err != nil {
		t.Fatal(err)
	}
	r := response{status: 200, header: h}
	for range 2 {
		w := httptest.NewRecorder()
		w.Header().Add("Vary", "Origin")
		w.Header().Add("X-A", "0")
		r.write(w)
		w.Header().Add("X-B", "3")
		if got := w.Header(); !reflect.DeepEqual(got, http.Header{"Vary": {"Origin", "Accept"}, "X-A": {"0", "1"}, "X-B": {"2", "3"}}) {
			t.Fatalf("client got %v", got)
		}
	}
	if want := (http.Header{"Vary": {"Accept"}, "X-A": {"1"}, "X-B": {"2"}}); !reflect.DeepEqual(r.header, want) {
		t.Fatalf("the response's header became %v, want %v", r.header, want)
	}
}

// The decoder must never read past the buffer or loop on garbage.
func FuzzDecodeAnswer(f *testing.F) {
	f.Add(wire.AppendHeader(wire.AppendInt(nil, 200), http.Header{"A": {"b"}}))
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _, _ = decodeAnswer(b)
	})
}
