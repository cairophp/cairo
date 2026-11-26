package test

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A plain front controller is served with no declaration: every path that
// names no file and no script runs it, as nginx's try_files does in front
// of PHP-FPM. The other classic tests serve the folder (classicWays).
func TestPlainFrontController(t *testing.T) {
	addr := freeAddr()
	base := startScripts(t, addr, []string{"../examples/classic/site/index.php", "--listen", addr, "--threads", "1"}, nil)
	for path, want := range map[string]string{
		"/":               "front / script=/index.php\n",
		"/pretty/link":    "front /pretty/link script=/index.php\n",
		"/blog/":          "blog index\n",
		"/robots.txt":     "User-agent: *\n",
		"/missing.php?x=": "front /missing.php?x= script=/index.php\n",
	} {
		if _, body := classicGet(t, http.DefaultClient, base+path); body != want {
			t.Errorf("%s: %q, want %q", path, body, want)
		}
	}
}

// cairo serve reads a file with PHP's tokenizer to tell a declaration from
// a plain script, and never runs it to find out.
func TestPlainTellsADeclaration(t *testing.T) {
	dir := t.TempDir()
	composer := filepath.Join(dir, "app", "composer.json")
	if err := os.MkdirAll(filepath.Join(dir, "app", "public"), 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, src string
		declares  bool
	}{
		{"use.php", "<?php use Cairo\\App; return new App();", true},
		{"alias.php", "<?php use Cairo\\App as A; return new A();", true},
		{"group.php", "<?php use Cairo\\{App, Context}; return new App();", true},
		{"qualified.php", "<?php return new \\Cairo\\App();", true},
		{"global.php", "<?php return new Cairo\\App();", true},
		{"imported.php", "<?php namespace X; use Cairo; return new Cairo\\App();", true},
		{"plain.php", "<?php echo 'hello';", false},
		{"comment.php", "<?php // new Cairo\\App()\necho 'Cairo\\App';", false},
		{"namespaced.php", "<?php namespace X; return new Cairo\\App();", false}, // X\Cairo\App
		{"closure.php", "<?php $f = function () use ($x) {}; class A { use T; }", false},
		{"app/public/index.php", "<?php require_once dirname(__DIR__).'/vendor/autoload_runtime.php'; return fn () => 1;", true},
	}
	if err := os.WriteFile(composer, []byte(`{"extra":{"runtime":{"class":"Cairo\\Symfony\\Runtime"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var args []string
	for _, c := range cases {
		file := filepath.Join(dir, filepath.FromSlash(c.name))
		if err := os.WriteFile(file, []byte(c.src), 0o644); err != nil {
			t.Fatal(err)
		}
		args = append(args, file)
	}
	out := declares(t, args)
	for i, c := range cases {
		if got := out[i] == "1"; got != c.declares {
			t.Errorf("%s: declares %v, want %v", c.name, got, c.declares)
		}
	}
	// With Symfony's own runtime the front controller is a plain one.
	if err := os.WriteFile(composer, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := declares(t, args[len(args)-1:]); out[0] != "0" {
		t.Errorf("a front controller with Symfony's runtime was taken for a declaration")
	}
}

// declares asks Cairo, through cairo php, whether each file declares an app.
func declares(t *testing.T, files []string) []string {
	t.Helper()
	cmd := exec.Command(cairoBin, append([]string{"php", "-r", `foreach (array_slice($argv, 1) as $f) echo (int) Cairo\Internal\Declares::app($f), "\n";`}, files...)...)
	cmd.Env = append(os.Environ(), "APP_RUNTIME=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return strings.Fields(string(out))
}

// --listen and --timeout set what has no declaration: a declaration names
// its own.
func TestPlainListenIsRefusedForADeclaration(t *testing.T) {
	out, err := exec.Command(cairoBin, "serve", "../examples/classic/server.php", "--listen", freeAddr()).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "--listen and --timeout are for a folder or a plain script") {
		t.Fatalf("%v: %s", err, out)
	}
}
