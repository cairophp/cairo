package core

import (
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Extensions depend on the core, never the reverse (AGENTS.md, "Core engine
// and extensions"). These tests hold the three ways the core could come to
// name an extension: a Go import, a C include and a name in its PHP files.
// A symbol used without one of them would not link, because this package's
// tests link the core with no extension.

// The core's Go packages import the standard library and each other.
func TestCoreImports(t *testing.T) {
	const module = "github.com/cairophp/cairo/"
	core := map[string]bool{module + "internal/core/php": true, module + "internal/spec": true, module + "internal/wire": true}
	for _, dir := range []string{".", "php", "../spec", "../wire"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no Go files in %s (%v)", dir, err)
		}
		for _, file := range files {
			f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, spec := range f.Imports {
				path, _ := strconv.Unquote(spec.Path.Value)
				if first, _, _ := strings.Cut(path, "/"); strings.Contains(first, ".") && !core[path] {
					t.Errorf("%s imports %s: the core imports only the standard library, internal/spec and internal/wire", file, path)
				}
			}
		}
	}
}

// The core's C code, in its C files and in the cgo comments of its Go files,
// includes its own headers by name. PHP's and the system's are in angle
// brackets.
func TestCoreIncludes(t *testing.T) {
	include := regexp.MustCompile(`(?m)^\s*#\s*include\s+"([^"]+)"`)
	var files []string
	for _, pattern := range []string{"*.c", "*.h", "*.go"} {
		found, _ := filepath.Glob(pattern)
		files = append(files, found...)
	}
	checked := 0
	for _, file := range files {
		code, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range include.FindAllStringSubmatch(string(code), -1) {
			checked++
			if m[1] == "_cgo_export.h" {
				continue
			}
			if _, err := os.Stat(m[1]); err != nil || filepath.Base(m[1]) != m[1] {
				t.Errorf("%s includes %q, which is not a file of the core", file, m[1])
			}
		}
	}
	if checked == 0 {
		t.Fatal("found no #include to check")
	}
}

// The core's PHP files name nothing that an extension declares. Cairo\App is
// the one exception: it is the declaration's public face, where server.php
// finds every setting and where PHP names the servers.
func TestCorePHPNames(t *testing.T) {
	const allowed = `internal/core/php/app.php: Cairo\Grpc\Server (php/grpc.php)
internal/core/php/app.php: Cairo\Hook (php/hooks.php)
internal/core/php/app.php: Cairo\Http\Server (php/http.php)
`
	php := phpCLI(t)
	out, err := exec.Command(php, "../../scripts/core-names.php").CombinedOutput()
	if err != nil {
		t.Fatalf("scripts/core-names.php: %v\n%s", err, out)
	}
	if string(out) != allowed {
		t.Errorf("the core's PHP files name these of the extensions:\n%s\nallowed are only:\n%s", out, allowed)
	}
}

// phpCLI finds a PHP command line binary with the tokenizer. Not finding one
// fails the test: what builds this package has PHP, and a check that skips
// holds nothing.
func phpCLI(t *testing.T) string {
	t.Helper()
	for _, php := range []string{os.Getenv("PHP_BIN"), "/opt/php-zts/bin/php", "php"} {
		if php == "" {
			continue
		}
		if out, err := exec.Command(php, "-r", "echo class_exists('PhpToken') ? 'y' : 'n';").Output(); err == nil && string(out) == "y" {
			return php
		}
	}
	t.Fatal("no php CLI with the tokenizer to read the PHP files with (set PHP_BIN)")
	return ""
}
