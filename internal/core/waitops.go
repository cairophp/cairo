package core

/*
#include <stddef.h>
*/
import "C"

import "unsafe"

// What PHP submits (submit in operation.go): C passes an operation's fields
// as arguments. These are the socket waits behind every hooked read, write
// and curl transfer, and the ops that carry a payload.

//export goFiberWait
func goFiberWait(tid C.int, request C.longlong, scope C.double, fd C.int, write C.int, timeout C.double) C.longlong {
	return engine.submit(tid, int64(request), float64(scope), op{Op: "wait", Fd: int(fd), Write: write != 0, Timeout: float64(timeout)})
}

//export goFiberWaitSet
func goFiberWaitSet(tid C.int, request C.longlong, scope C.double, read *C.int, nr C.int, write *C.int, nw C.int, timeout C.double) C.longlong {
	return engine.submit(tid, int64(request), float64(scope), op{Op: "waitset", Read: cInts(read, nr), WriteFD: cInts(write, nw), Timeout: float64(timeout)})
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

// goFiberCall submits an operation that carries a payload (Go::call ops,
// "pg", "offload"). The payload is passed as is: wrapping it in the op's
// JSON escaped it once in PHP and unescaped it again here.
//
//export goFiberCall
func goFiberCall(tid C.int, request C.longlong, scope C.double, name *C.char, nn C.int, payload *C.char, np C.int) C.longlong {
	return engine.submit(tid, int64(request), float64(scope), op{Op: C.GoStringN(name, nn), Payload: C.GoStringN(payload, np)})
}
