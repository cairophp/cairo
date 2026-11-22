#ifndef CAIRO_GRPC_ANSWER_H
#define CAIRO_GRPC_ANSWER_H

#include "extension.h"

// cairo_respond_grpc(), which php/grpc_answer.php ends every gRPC call
// with; grpcanswer.go adds it to the SAPI module.
extern const cairo_extension cairo_grpc_answer;

#endif
