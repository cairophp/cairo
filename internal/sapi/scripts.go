package sapi

/*
#include "http_input.h"
*/
import "C"

// The scripts of $http->scripts() in classic mode: the PHP half that runs
// the script Go named for a request (its Go half is internal/httpserver's
// scripts.go), and what gives the script its php://input.
func init() {
	registerPHP("scripts.php", `Cairo\Internal\Scripts`)
	registerC(&C.cairo_http_input)
}
