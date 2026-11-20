package core

import (
	"testing"
	"testing/fstest"
)

// Every PHP file of the runtime is written into one directory, so an
// extension cannot add a file under a name that is there already: it would
// replace the core's own.
func TestRegisterPHPTakenName(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("an extension added its own runtime.php")
		}
	}()
	RegisterPHP(fstest.MapFS{"runtime.php": &fstest.MapFile{}}, "runtime.php")
}
