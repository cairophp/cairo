// Package sapi holds the extensions that are compiled into the cairo binary,
// for the part of each that lives next to the embedded PHP: its Go
// operations, its C code and what registers its PHP file. The HTTP and gRPC
// servers have their Go halves in internal/httpserver and
// internal/grpcserver. Every file joins the core engine (internal/core) from
// its init(), so importing the package is what compiles the extensions in.
package sapi

/*
#cgo CFLAGS: -D_GNU_SOURCE -I${SRCDIR}/../core
#cgo LDFLAGS: -lphp -lcurl
#include "extension.h"
*/
import "C"

import (
	"unsafe"

	"github.com/cairophp/cairo/internal/core"
)

// registerC adds the C side of an extension of this package to the SAPI
// module (core.RegisterC); call it from init() with the cairo_extension the
// extension's header declares.
func registerC(ext *C.cairo_extension) {
	core.RegisterC(unsafe.Pointer(ext))
}
