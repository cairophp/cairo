#ifndef CAIRO_GRPC_STREAM_H
#define CAIRO_GRPC_STREAM_H

#include "extension.h"

// cairo_grpc_send() and cairo_grpc_client_send(), which the send() of
// Cairo\Grpc\ServerStream and of ClientStream queue each message with;
// grpcsend.go adds them to the SAPI module.
extern const cairo_extension cairo_grpc_stream;

#endif
