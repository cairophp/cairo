package httpserver

import (
	"errors"
	"io"
	"net/http"
)

// bodyStep is how much of the length a client declares readBody takes on
// trust.
const bodyStep = 1 << 20

var errBodyTooLarge = errors.New("request body too large")

// readBody reads the request's body for PHP, maxBody at most. The declared
// length sizes the buffer, so the body is read into one allocation and not
// into one that grows and is copied as the bytes arrive. Only bodyStep of
// that length is taken on trust: past it the buffer doubles as it fills, so
// a client makes Cairo reserve at most twice what it has sent.
func readBody(w http.ResponseWriter, req *http.Request) ([]byte, error) {
	n := req.ContentLength
	switch {
	case n == 0:
		return nil, nil
	case n > maxBody:
		return nil, errBodyTooLarge
	case n < 0: // chunked: the length is not known
		return io.ReadAll(http.MaxBytesReader(w, req.Body, maxBody))
	}
	buf := make([]byte, min(n, bodyStep))
	for read := 0; ; {
		m, err := io.ReadFull(req.Body, buf[read:])
		if err != nil {
			return nil, err // the client sent less than it declared, or went away
		}
		if read += m; int64(read) == n {
			return buf, nil
		}
		grown := make([]byte, min(n, 2*int64(read)))
		copy(grown, buf)
		buf = grown
	}
}
