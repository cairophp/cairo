package sapi

import (
	"github.com/cairophp/cairo/internal/core"
	"github.com/cairophp/cairo/php"
)

// registerPHP adds php/<name> to the runtime (core.RegisterPHP); call it
// from init(). Every PHP thread loads the file at start or, when classes are
// named, on the first use of one of them.
func registerPHP(name string, classes ...string) {
	core.RegisterPHP(php.Files, name, classes...)
}
