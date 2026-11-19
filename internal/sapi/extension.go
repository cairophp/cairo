package sapi

/*
#include "extension.h"
*/
import "C"

// registerC adds the C side of an extension to the SAPI module: what to run
// at module startup and when a PHP thread ends, and the PHP functions it
// brings. Call it from init() with the cairo_extension the extension's
// header declares.
func registerC(ext *C.cairo_extension) {
	if C.cairo_extension_add(ext) != 0 {
		panic("sapi: too many C extensions, raise CAIRO_MAX_EXTENSIONS")
	}
}
