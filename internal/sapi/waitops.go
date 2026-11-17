package sapi

/*
#include <stddef.h>
*/
import "C"

import "unsafe"

// The socket waits behind every hooked read, write and curl transfer are the
// most frequent operations, so they skip goFiberOp's JSON: C passes the
// fields as arguments.

//export goFiberWait
func goFiberWait(id C.longlong, fd C.int, write C.int, timeout C.double) C.longlong {
	return engine.submit(int64(id), op{Op: "wait", Fd: int(fd), Write: write != 0, Timeout: float64(timeout)})
}

//export goFiberWaitSet
func goFiberWaitSet(id C.longlong, read *C.int, nr C.int, write *C.int, nw C.int, timeout C.double) C.longlong {
	return engine.submit(int64(id), op{Op: "waitset", Read: cInts(read, nr), WriteFD: cInts(write, nw), Timeout: float64(timeout)})
}

func cInts(p *C.int, n C.int) []int {
	if n <= 0 {
		return nil
	}
	out := make([]int, n)
	for i, v := range unsafe.Slice(p, int(n)) {
		out[i] = int(v)
	}
	return out
}
