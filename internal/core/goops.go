package core

/*
#include <stdlib.h>
*/
import "C"

import (
	"context"
	"unsafe"

	"github.com/cairophp/cairo/internal/wire"
)

// GoOp is a Go operation that PHP APIs call through
// Cairo\Internal\Go::call(). Inside a request fiber it runs in a goroutine
// of its own with the request's context while the fiber is suspended;
// elsewhere (the declare phase, offload threads) the PHP thread waits for it
// with a background context. The payload is the array PHP passed, in
// internal/wire's form: wire.Unmarshal reads it as encoding/json would its
// JSON. The result goes back to PHP the same way. An error is raised in PHP
// as a RuntimeException, so operations report expected failures inside
// their result instead.
type GoOp func(ctx context.Context, payload []byte) (any, error)

var goOps = map[string]GoOp{}

// RegisterOp adds an operation; call it from init().
func RegisterOp(name string, fn GoOp) {
	goOps[name] = fn
}

// FiberOp is an operation that only a fiber can ask for: what PHP's hooked
// functions (connect, TLS, DNS, PDO pgsql) do in a goroutine while the fiber
// is suspended. It returns what the fiber wakes up with: a result,
// or the text of an error. Unlike a GoOp it decides itself what the end of
// the context means, because a socket that did connect must still reach PHP,
// which owns it from then on.
type FiberOp func(ctx context.Context, payload []byte) (result any, errText string)

var fiberOps = map[string]FiberOp{}

// RegisterFiberOp adds a fiber operation; call it from init().
func RegisterFiberOp(name string, fn FiberOp) {
	fiberOps[name] = fn
}

// goCall runs an operation on the calling PHP thread. Returns a map with
// "result" or "error" in internal/wire's form: n bytes of C memory, which
// the caller frees.
//
//export goCall
func goCall(name *C.char, data *C.char, dn C.int, n *C.int) unsafe.Pointer {
	b := wire.AppendMap(nil, 1)
	if h, ok := goOps[C.GoString(name)]; !ok {
		b = wire.AppendString(wire.AppendKey(b, "error"), "unknown operation "+C.GoString(name))
	} else if res, err := h(context.Background(), C.GoBytes(unsafe.Pointer(data), dn)); err != nil {
		b = wire.AppendString(wire.AppendKey(b, "error"), err.Error())
	} else {
		b = wire.AppendValue(wire.AppendKey(b, "result"), res)
	}
	*n = C.int(len(b))
	return C.CBytes(b)
}
