package sapi

import (
	"io/fs"

	"github.com/cairophp/cairo/php"
)

// The PHP files that extensions add to the runtime. runtime.php loads the
// core's own files itself and learns about these from cairo_info().
var (
	phpAtStart = []string{}          // loaded by every PHP thread at start, in this order
	phpOnUse   = map[string]string{} // class => the file that its first use loads
)

// registerPHP adds the embedded file php/<name>; call it from init(). Every
// PHP thread loads the file at start or, when classes are named, on the first
// use of one of them. A file must load without another extension's.
func registerPHP(name string, classes ...string) {
	if _, err := fs.Stat(php.Files, name); err != nil {
		panic("sapi: registerPHP: " + err.Error())
	}
	if len(classes) == 0 {
		phpAtStart = append(phpAtStart, name)
	}
	for _, class := range classes {
		phpOnUse[class] = name
	}
}
