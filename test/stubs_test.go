package test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// stubs/cairo.php, the IDE stubs shipped by the Composer package, is
// generated from the files in php/ and must be up to date. It must also load in
// plain PHP, with no Cairo\Internal names in it.
func TestStubsUpToDate(t *testing.T) {
	php, err := exec.LookPath("php")
	if err != nil {
		t.Skip("no php CLI on PATH")
	}
	if out, _ := exec.Command(php, "-r", "echo class_exists('PhpToken') ? 'y' : 'n';").Output(); string(out) != "y" {
		t.Skip("the php CLI has no tokenizer")
	}
	want, err := exec.Command(php, "../scripts/stubs.php").Output()
	if err != nil {
		t.Fatalf("scripts/stubs.php: %v", err)
	}
	have, err := os.ReadFile("../stubs/cairo.php")
	if err != nil {
		t.Fatal(err)
	}
	if string(have) != string(want) {
		t.Fatal("stubs/cairo.php is stale: run make stubs")
	}
	if strings.Contains(string(have), "Cairo\\Internal") {
		t.Fatal("stubs/cairo.php mentions Cairo\\Internal")
	}
	out, err := exec.Command(php, "-r", `require "../stubs/cairo.php"; echo class_exists('Cairo\App') && function_exists('Cairo\async') ? 'ok' : 'missing';`).CombinedOutput()
	if err != nil || string(out) != "ok" {
		t.Fatalf("loading stubs/cairo.php: %v %s", err, out)
	}
}
