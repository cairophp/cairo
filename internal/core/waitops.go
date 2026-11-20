package core

/*
#include <stddef.h>
*/
import "C"

import "unsafe"

// The most frequent fiber operations skip goFiberOp's JSON: C passes their
// fields as arguments. These are the socket waits behind every hooked read,
// write and curl transfer, and the ops that carry a payload.

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

// goFiberCall submits an operation that carries a payload (Go::call ops,
// "pg", "offload"). The payload is passed as is: wrapping it in the op's
// JSON escaped it once in PHP and unescaped it again here.
//
//export goFiberCall
func goFiberCall(id C.longlong, name *C.char, nn C.int, payload *C.char, np C.int) C.longlong {
	return engine.submit(int64(id), op{Op: C.GoStringN(name, nn), Payload: C.GoStringN(payload, np)})
}

// goInboxLen reports how many events wait for the PHP thread: cairo_wait_here()
// does not hold the thread while it has other work.
//
//export goInboxLen
func goInboxLen(tid C.int) C.int {
	t := engine.table[int(tid)].Load()
	if t == nil {
		return 0
	}
	return C.int(len(t.inbox))
}
