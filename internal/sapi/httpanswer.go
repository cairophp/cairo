package sapi

/*
#include "http_answer.h"
*/
import "C"

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
