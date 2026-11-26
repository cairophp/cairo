package sapi

/*
#include "http_input.h"
#include "plain_request.h"
*/
import "C"

// The scripts of $http->scripts() in classic mode: the PHP half that runs
// the script Go named for a request (its Go half is internal/httpserver's
// scripts.go), and what gives the script its php://input.
func init() {
	registerPHP("scripts.php", `Cairo\Internal\Scripts`)
	// cairo serve's entry, for a folder or a script with no declaration.
	registerPHP("plain.php", `Cairo\Internal\Plain`)
	// What a script served in C loads of Cairo (plain_request.c).
	registerPHP("plain_request.php", `Cairo\Internal\PlainRequest`)
	registerPHP("declares.php", `Cairo\Internal\Declares`)
	registerC(&C.cairo_http_input)
	registerC(&C.cairo_plain_request)
}
