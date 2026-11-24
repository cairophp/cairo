#ifndef CAIRO_HTTP_STREAM_H
#define CAIRO_HTTP_STREAM_H

#include "extension.h"

// The streamed answer of an HTTP request, on its way from PHP to Go:
// cairo_http_send(), which Cairo\Http\Stream queues each piece with, and
// what a fiber writes with echo while its request is answered that way
// (http_stream.c). httpstream.go adds it to the SAPI module.
extern const cairo_extension cairo_http_stream;

#endif
