package test

import (
	"debug/elf"
	"os"
	"testing"
)

// TestStaticBinary checks the binary from scripts/build-static.sh: it has no
// dynamic loader and needs no shared library. The rest of the suite runs
// against the same binary (CAIRO_TEST_BIN).
func TestStaticBinary(t *testing.T) {
	if os.Getenv("CAIRO_TEST_STATIC") == "" {
		t.Skip("set CAIRO_TEST_STATIC=1 and CAIRO_TEST_BIN to a static build")
	}
	f, err := elf.Open(cairoBin)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			t.Fatalf("%s has a dynamic loader (PT_INTERP): not static", cairoBin)
		}
	}
	if libs, _ := f.ImportedLibraries(); len(libs) > 0 {
		t.Fatalf("%s needs shared libraries: %v", cairoBin, libs)
	}
	if raw := get(t, "/php/extensions"); raw == "" {
		t.Fatal("the static binary did not serve PHP")
	}
}
