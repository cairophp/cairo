package httpserver

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
)

func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i * 7)
	}
	return b
}

func TestReadBody(t *testing.T) {
	for _, n := range []int{0, 1, 4096, bodyStep - 1, bodyStep, bodyStep + 1, 3*bodyStep + 5} {
		want := pattern(n)
		// With the length declared, and without (chunked).
		for _, body := range []io.Reader{bytes.NewReader(want), io.MultiReader(bytes.NewReader(want))} {
			req := httptest.NewRequest("POST", "/", body)
			got, err := readBody(httptest.NewRecorder(), req)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("%d bytes (declared %d): got %d bytes, %v", n, req.ContentLength, len(got), err)
			}
		}
	}
}

// The length is the client's word: a body shorter than it is an error, and
// what Cairo reserved for it stays near what the client sent.
func TestReadBodyDoesNotTrustTheLength(t *testing.T) {
	const sent = 3 * bodyStep
	req := httptest.NewRequest("POST", "/", io.LimitReader(&countingReader{}, sent))
	req.ContentLength = maxBody
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := readBody(httptest.NewRecorder(), req)
	runtime.ReadMemStats(&after)
	if err == nil {
		t.Fatal("read a body that ended before its declared length")
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > 4*sent {
		t.Fatalf("allocated %d bytes for a body of %d that declared %d", got, sent, maxBody)
	}
}

func TestReadBodyRefusesALengthPastTheLimit(t *testing.T) {
	body := &countingReader{}
	req := httptest.NewRequest("POST", "/", body)
	req.ContentLength = maxBody + 1
	if _, err := readBody(httptest.NewRecorder(), req); err == nil {
		t.Fatal("accepted a declared length past the limit")
	}
	if body.n != 0 {
		t.Fatalf("read %d bytes of a body that is refused for its length", body.n)
	}
	w := httptest.NewRecorder()
	if _, _, ok := newEvent(w, req, Route{ID: 1}, nil); ok || w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("newEvent: ok %v, status %d, want 413", ok, w.Code)
	}
}
