package core

/*
#include "extension.h"
*/
import "C"

import "unsafe"

// RegisterC adds the C side of an extension to the SAPI module: what to run
// at module startup and when a PHP request of a thread ends, and the PHP functions it
// brings. Call it from init() with the address of the cairo_extension the
// extension's header declares. It crosses as an unsafe.Pointer because cgo
// gives every package its own C types.
func RegisterC(ext unsafe.Pointer) {
	if C.cairo_extension_add((*C.cairo_extension)(ext)) != 0 {
		panic("core: too many C extensions, raise CAIRO_MAX_EXTENSIONS")
	}
}
