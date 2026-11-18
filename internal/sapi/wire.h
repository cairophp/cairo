#ifndef CAIRO_WIRE_H
#define CAIRO_WIRE_H

#include <php.h>
#include <zend_smart_str.h>

// Decodes one internal/wire value filling all of buf into out. Returns
// SUCCESS, or FAILURE (out is then undefined) for a malformed buffer.
int cairo_wire_decode(const char *buf, size_t len, zval *out);

// Appends an HTTP response's status (an int) and headers (a map of lists of
// strings; a non-array value is a list of one) in internal/wire's format.
// Returns FAILURE when a header value cannot be turned into a string (an
// exception is then pending).
int cairo_wire_encode_response(zend_long status, HashTable *headers, smart_str *out);

#endif
