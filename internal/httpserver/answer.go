package httpserver

import (
	"encoding/binary"
	"errors"
	"net/http"

	"github.com/cairophp/cairo/internal/wire"
)

// errMalformed is returned for an answer that does not hold what was expected.
var errMalformed = errors.New("wire: malformed buffer")

// decodeAnswer reads what cairo_respond_http() sends for a request
// (internal/sapi/http_answer.c), with internal/wire's tags: the status as
// TagInt, then the headers as a map of lists of strings.
func decodeAnswer(b []byte) (status int, header http.Header, err error) {
	if len(b) < 9 || b[0] != wire.TagInt {
		return 0, nil, errMalformed
	}
	status = int(int64(binary.LittleEndian.Uint64(b[1:])))
	b = b[9:]
	n, b, err := count(b, wire.TagMap)
	if err != nil {
		return 0, nil, err
	}
	header = make(http.Header, n)
	for range n {
		var key []byte
		if key, b, err = chunk(b); err != nil {
			return 0, nil, err
		}
		var m uint32
		if m, b, err = count(b, wire.TagList); err != nil {
			return 0, nil, err
		}
		vals := make([]string, 0, m)
		for range m {
			if len(b) == 0 || b[0] != wire.TagString {
				return 0, nil, errMalformed
			}
			var v []byte
			if v, b, err = chunk(b[1:]); err != nil {
				return 0, nil, err
			}
			vals = append(vals, string(v))
		}
		k := string(key)
		header[k] = append(header[k], vals...)
	}
	if len(b) != 0 {
		return 0, nil, errMalformed
	}
	return status, header, nil
}

// count reads a list or map header with the given tag. Every entry takes at
// least a byte, so a count beyond what is left is malformed.
func count(b []byte, tag byte) (uint32, []byte, error) {
	if len(b) < 5 || b[0] != tag {
		return 0, nil, errMalformed
	}
	n := binary.LittleEndian.Uint32(b[1:])
	if uint64(n) > uint64(len(b)-5) {
		return 0, nil, errMalformed
	}
	return n, b[5:], nil
}

// chunk reads a uint32 length and that many bytes.
func chunk(b []byte) ([]byte, []byte, error) {
	if len(b) < 4 {
		return nil, nil, errMalformed
	}
	n := binary.LittleEndian.Uint32(b)
	if uint64(n) > uint64(len(b)-4) {
		return nil, nil, errMalformed
	}
	return b[4 : 4+n], b[4+n:], nil
}
