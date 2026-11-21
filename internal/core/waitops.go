package core

/*
#include <stddef.h>
*/
import "C"

// What PHP submits (submit in operation.go): C passes an operation's fields
// as arguments.

// goFiberCall submits an operation that carries a payload (Go::call ops,
// "pg"). The payload is passed as is: wrapping it in the op's
// JSON escaped it once in PHP and unescaped it again here.
//
//export goFiberCall
func goFiberCall(tid C.int, request C.longlong, scope C.double, name *C.char, nn C.int, payload *C.char, np C.int) C.longlong {
	return engine.submit(tid, int64(request), float64(scope), op{Op: C.GoStringN(name, nn), Payload: C.GoStringN(payload, np)})
}
