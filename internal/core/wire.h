#ifndef CAIRO_WIRE_H
#define CAIRO_WIRE_H

#include <php.h>
#include <zend_smart_str.h>

// The tags of internal/wire's values. Keep in sync with internal/wire/wire.go.
enum {
	TAG_NULL = 0, TAG_FALSE, TAG_TRUE, TAG_INT, TAG_FLOAT, TAG_STRING, TAG_LIST, TAG_MAP, TAG_JSON
};

// Decodes one internal/wire value filling all of buf into out. Returns
// SUCCESS, or FAILURE (out is then undefined) for a malformed buffer.
int cairo_wire_decode(const char *buf, size_t len, zval *out);

// Appends the arguments of a Go operation to out as one internal/wire value,
// which wire.Unmarshal reads in Go. Returns FAILURE, with an exception
// pending, for what cannot cross: a resource, a string of 4 GB, an object
// that PHP's json extension refuses, or nesting deeper than the decoder
// allows. The caller frees out either way.
int cairo_wire_encode(HashTable *args, smart_str *out);

// The same for one value of any type that can cross.
int cairo_wire_encode_value(zval *value, smart_str *out);

#endif
