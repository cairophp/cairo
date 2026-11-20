package sapi

/*
#include "http_answer.h"
*/
import "C"

import (
	"unsafe"

	"github.com/cairophp/cairo/internal/core"
)

// The HTTP server's half in this package (its Go half is internal/httpserver,
// which sends each request through Engine.Dispatch): the PHP files of its
// API and of how a request is answered, and the C function that hands the
// response to Go.
func init() {
	registerPHP("http.php")
	registerPHP("form.php")
	registerPHP("superglobals.php")
	registerPHP("http_adapter.php")
	registerPHP("http_answer.php")
	registerC(&C.cairo_http_answer)
}

// goRespondHTTP is where cairo_respond_http() hands an answer to Go: the
// status and headers as http_answer.c encodes them, and the body.
//
//export goRespondHTTP
func goRespondHTTP(id C.longlong, meta *C.char, n C.int, body *C.char, bn C.int) {
	core.Respond(int64(id), unsafe.Pointer(meta), int(n), unsafe.Pointer(body), int(bn))
}
