package sapi

/*
#include "curl_hook.h"
*/
import "C"

// The curl hook is all C (curl_hook.c): libphp's calls into libcurl land in
// it. This file only adds it to the SAPI module.
func init() {
	registerC(&C.cairo_curl_hook)
}
