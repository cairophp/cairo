package core

import (
	"io/fs"
	"os"
	"path/filepath"

	"github.com/cairophp/cairo/internal/core/php"
)

// The PHP files of the runtime: the core's own, which runtime.php loads
// itself, and those that extensions add, which it learns from cairo_info().
var (
	phpSources = map[string]fs.FS{}  // file name => where the file is embedded
	phpAtStart = []string{}          // extensions' files that every PHP thread loads at start, in this order
	phpOnUse   = map[string]string{} // class => the extension's file that its first use loads
)

func init() {
	own, err := php.Files.ReadDir(".")
	if err != nil {
		panic("core: " + err.Error())
	}
	for _, file := range own {
		phpSources[file.Name()] = php.Files
	}
}

// RegisterPHP adds the file name of files, where an extension embeds its PHP;
// call it from init(). Every PHP thread loads the file at start or, when
// classes are named, on the first use of one of them. A file must load
// without another extension's. The files of all extensions are written next
// to the core's (writeRuntime), so a name can be taken once.
func RegisterPHP(files fs.FS, name string, classes ...string) {
	if _, err := fs.Stat(files, name); err != nil {
		panic("core: RegisterPHP: " + err.Error())
	}
	if _, taken := phpSources[name]; taken {
		panic("core: RegisterPHP: there is a " + name + " in the runtime already")
	}
	phpSources[name] = files
	if len(classes) == 0 {
		phpAtStart = append(phpAtStart, name)
	}
	for _, class := range classes {
		phpOnUse[class] = name
	}
}

// writeRuntime puts the embedded PHP files, the core's and the extensions',
// in one directory of their own, since PHP runs files, and returns the path
// of the one every PHP thread runs.
func writeRuntime() (string, error) {
	dir, err := os.MkdirTemp("", "cairo-runtime-")
	if err != nil {
		return "", err
	}
	for name, files := range phpSources {
		code, err := fs.ReadFile(files, name)
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(dir, name), code, 0o600); err != nil {
			return "", err
		}
	}
	return filepath.Join(dir, "runtime.php"), nil
}
