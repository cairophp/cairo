#ifndef CAIRO_WIRE_H
#define CAIRO_WIRE_H

#include <php.h>

// Decodes one internal/wire value filling all of buf into out. Returns
// SUCCESS, or FAILURE (out is then undefined) for a malformed buffer.
int cairo_wire_decode(const char *buf, size_t len, zval *out);

#endif
