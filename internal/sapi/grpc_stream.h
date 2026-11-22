#ifndef CAIRO_GRPC_STREAM_H
#define CAIRO_GRPC_STREAM_H

#include "extension.h"

// cairo_grpc_send(), which Cairo\Grpc\ServerStream::send() sends each reply
// with; grpcsend.go adds it to the SAPI module.
extern const cairo_extension cairo_grpc_stream;

#endif
