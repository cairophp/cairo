package core

/*
#include <stdlib.h>
*/
import "C"

import (
	"context"
	"encoding/json"
	"unsafe"
)

// GoOp is a Go operation that PHP APIs (Cairo\Http\Client, ...) call through
// Cairo\Internal\Go::call(). Inside a request fiber it runs on the fiber's
// goroutine with the request's context while the fiber is suspended;
// elsewhere (the declare phase, offload threads) the PHP thread waits for it
// with a background context. The payload is the JSON PHP sent; the result is
// marshalled back to PHP. An error is raised in PHP as a RuntimeException,
// so operations report expected failures inside their result instead.
type GoOp func(ctx context.Context, payload []byte) (any, error)

var goOps = map[string]GoOp{}

// RegisterOp adds an operation; call it from init().
func RegisterOp(name string, fn GoOp) {
	goOps[name] = fn
}

// FiberOp is an operation that only a fiber can ask for: what PHP's hooked
// functions (connect, TLS, DNS, PDO pgsql) do on the fiber's goroutine while
// the fiber is suspended. It returns what the fiber wakes up with: a result,
// or the text of an error. Unlike a GoOp it decides itself what the end of
// the context means, because a socket that did connect must still reach PHP,
// which owns it from then on.
type FiberOp func(ctx context.Context, payload []byte) (result any, errText string)

var fiberOps = map[string]FiberOp{}

// RegisterFiberOp adds a fiber operation; call it from init().
func RegisterFiberOp(name string, fn FiberOp) {
	fiberOps[name] = fn
}

// goCall runs an operation on the calling PHP thread. Returns JSON
// {"result": ...} or {"error": "..."}; the caller frees it.
//
//export goCall
func goCall(name *C.char, data *C.char, n C.int) *C.char {
	resp := map[string]any{}
	if h, ok := goOps[C.GoString(name)]; !ok {
		resp["error"] = "unknown operation " + C.GoString(name)
	} else if res, err := h(context.Background(), C.GoBytes(unsafe.Pointer(data), n)); err != nil {
		resp["error"] = err.Error()
	} else {
		resp["result"] = res
	}
	b, err := json.Marshal(resp)
	if err != nil {
		b, _ = json.Marshal(map[string]any{"error": err.Error()})
	}
	return C.CString(string(b))
}
