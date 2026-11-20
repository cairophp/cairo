package core

/*
// Sends this binary's calls of pthread_getattr_np() to stackbound.c.
#cgo LDFLAGS: -Wl,--wrap=pthread_getattr_np
#include "stackbound.h"
*/
import "C"

// stackBound asks for the calling thread's stack the way Go's runtime does
// on a call from C, and says how many of the thread's questions went on to
// glibc. It is here for the test: a test file cannot use cgo.
func stackBound() (lo, hi uintptr, lookups int) {
	var bounds [2]C.uintptr_t
	n := C.cairo_stackbound_probe(&bounds[0])
	return uintptr(bounds[0]), uintptr(bounds[1]), int(n)
}
