#ifndef CAIRO_WIRE_H
#define CAIRO_WIRE_H

#include <php.h>

// The tags of internal/wire's values. Keep in sync with internal/wire/wire.go.
enum {
	TAG_NULL = 0, TAG_FALSE, TAG_TRUE, TAG_INT, TAG_FLOAT, TAG_STRING, TAG_LIST, TAG_MAP, TAG_JSON
};

// Decodes one internal/wire value filling all of buf into out. Returns
// SUCCESS, or FAILURE (out is then undefined) for a malformed buffer.
int cairo_wire_decode(const char *buf, size_t len, zval *out);

#endif
