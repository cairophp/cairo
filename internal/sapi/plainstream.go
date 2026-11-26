package sapi

import "C"

import (
	"unsafe"

	"github.com/cairophp/cairo/internal/core"
	"github.com/cairophp/cairo/internal/httpserver"
)

// What a script served in C (plain_request.c) does with its client before
// it answers: flush(), 1 MiB of output, headers_send(). The script waits
// with its thread here, as under PHP-FPM, under the call's context: a
// client that reads slowly holds it back until the deadline at most.

// goPlainOpen sends the head of call's answer (head is encoded as an
// answer's) and returns the id of the stream its body goes on, or -1.
//
//export goPlainOpen
func goPlainOpen(tid C.int, call C.longlong, head *C.char, n C.int) C.longlong {
	ctx := core.CallContext(int(tid), int64(call))
	if ctx == nil {
		return -1
	}
	id, err := httpserver.OpenStreamOf(ctx, C.GoBytes(unsafe.Pointer(head), n))
	if err != nil {
		return -1
	}
	return C.longlong(id)
}

// goPlainSend sends a piece of the body on stream, waiting for room: 0, or
// -1 once the client or the call is gone.
//
//export goPlainSend
func goPlainSend(tid C.int, call, stream C.longlong, data *C.char, n C.int) C.int {
	ctx := core.CallContext(int(tid), int64(call))
	if ctx == nil || httpserver.Send(ctx, int64(stream), C.GoBytes(unsafe.Pointer(data), n)) != nil {
		return -1
	}
	return 0
}

// goPlainHint sends an informational answer (103 Early Hints) to call's
// client: 0, or -1.
//
//export goPlainHint
func goPlainHint(tid C.int, call C.longlong, head *C.char, n C.int) C.int {
	ctx := core.CallContext(int(tid), int64(call))
	if ctx == nil || httpserver.HintOf(ctx, C.GoBytes(unsafe.Pointer(head), n)) != nil {
		return -1
	}
	return 0
}
