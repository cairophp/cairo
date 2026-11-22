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
//
// The header it returns has canonical names, and its strings are parts of one
// copy of b: an answer costs that copy, the map and one slice of values,
// however many headers it has. No value slice has spare capacity, so one can
// be shared with a response's header and an append to it there copies it.
func decodeAnswer(b []byte) (status int, header http.Header, err error) {
	if len(b) < 9 || b[0] != wire.TagInt {
		return 0, nil, errMalformed
	}
	status = int(int64(binary.LittleEndian.Uint64(b[1:])))
	s := string(b[9:])
	n, s, err := count(s, wire.TagMap)
	if err != nil {
		return 0, nil, err
	}
	header = make(http.Header, n)
	vals := make([]string, 0, n) // one value for a name is the usual case
	for range n {
		var key string
		if key, s, err = chunk(s); err != nil {
			return 0, nil, err
		}
		var m uint32
		if m, s, err = count(s, wire.TagList); err != nil {
			return 0, nil, err
		}
		if int(m) > cap(vals)-len(vals) {
			vals = make([]string, 0, m)
		}
		first := len(vals)
		for range m {
			if len(s) == 0 || s[0] != wire.TagString {
				return 0, nil, errMalformed
			}
			var v string
			if v, s, err = chunk(s[1:]); err != nil {
				return 0, nil, err
			}
			vals = append(vals, v)
		}
		k := http.CanonicalHeaderKey(key)
		if old, ok := header[k]; ok { // PHP sent the name twice, in two spellings
			header[k] = append(old, vals[first:]...)
			header[k] = header[k][:len(header[k]):len(header[k])]
			continue
		}
		header[k] = vals[first:len(vals):len(vals)]
	}
	if len(s) != 0 {
		return 0, nil, errMalformed
	}
	return status, header, nil
}

// count reads a list or map header with the given tag. Every entry takes at
// least a byte, so a count beyond what is left is malformed.
func count(s string, tag byte) (uint32, string, error) {
	if len(s) < 5 || s[0] != tag {
		return 0, "", errMalformed
	}
	n := uint32(s[1]) | uint32(s[2])<<8 | uint32(s[3])<<16 | uint32(s[4])<<24
	if uint64(n) > uint64(len(s)-5) {
		return 0, "", errMalformed
	}
	return n, s[5:], nil
}

// chunk reads a uint32 length and that many bytes.
func chunk(s string) (string, string, error) {
	if len(s) < 4 {
		return "", "", errMalformed
	}
	n := uint32(s[0]) | uint32(s[1])<<8 | uint32(s[2])<<16 | uint32(s[3])<<24
	if uint64(n) > uint64(len(s)-4) {
		return "", "", errMalformed
	}
	return s[4 : 4+n], s[4+n:], nil
}
