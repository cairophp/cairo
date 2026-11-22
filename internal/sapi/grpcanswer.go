package sapi

/*
#include "grpc_answer.h"
*/
import "C"

// How a gRPC call is answered: the PHP file that runs the call's method,
// and the C function that hands its reply to Go.
func init() {
	registerPHP("grpc_answer.php")
	registerC(&C.cairo_grpc_answer)
}
