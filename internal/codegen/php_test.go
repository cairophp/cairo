package codegen

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

// The example's committed classes match what cairo gen writes today.
func TestExampleIsFresh(t *testing.T) {
	files, err := Generate([]string{"../../examples/greeter/proto/helloworld.proto"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		got, err := os.ReadFile(filepath.Join("../../examples/greeter/gen", f.Path))
		if err != nil {
			t.Fatalf("%s: %v (run: cairo gen --out examples/greeter/gen examples/greeter/proto/helloworld.proto)", f.Path, err)
		}
		if string(got) != f.Content {
			t.Fatalf("%s is stale; regenerate it with cairo gen", f.Path)
		}
	}
}

func phpBinary(t *testing.T) string {
	for _, p := range []string{os.Getenv("PHP_BIN"), "/opt/php-zts/bin/php"} {
		if p != "" {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	if p, err := exec.LookPath("php"); err == nil {
		return p
	}
	t.Skip("no php binary (set PHP_BIN)")
	return ""
}

// A protojson array goes through fromArray() and toArray() unchanged, with
// enums, 64-bit ints, bytes, maps, nested and repeated messages, optional and
// oneof fields, and a well-known type.
func TestGeneratedClassesRoundTrip(t *testing.T) {
	php := phpBinary(t)
	files, err := Generate([]string{"testdata/shop.proto"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := Write(dir, files); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		out, err := exec.Command(php, "-l", filepath.Join(dir, f.Path)).CombinedOutput()
		if err != nil {
			t.Fatalf("php -l %s: %s", f.Path, out)
		}
	}

	in := map[string]any{
		"id":        "o-1",
		"items":     []any{map[string]any{"sku": "a", "qty": "9007199254740993", "blob": "aGk="}},
		"tags":      map[string]any{"x": float64(1)},
		"status":    "STATUS_PAID",
		"note":      "fragile",
		"createdAt": "2026-11-15T12:00:00Z",
		"ship":      map[string]any{"city": "Cairo"},
		"card":      "4242",
	}
	want := map[string]any{
		"id":        "o-1",
		"items":     []any{map[string]any{"sku": "a", "qty": "9007199254740993", "blob": "aGk="}},
		"tags":      map[string]any{"x": float64(1)},
		"status":    float64(1), // enums go back as numbers, which protojson accepts
		"note":      "fragile",
		"createdAt": "2026-11-15T12:00:00Z",
		"ship":      map[string]any{"city": "Cairo"},
		"card":      "4242",
	}
	input, _ := json.Marshal(in)
	script := `<?php
require $argv[1] . '/autoload.php';
$o = \Shop\V1\Order::fromArray(json_decode($argv[2], true));
assert($o->items[0] instanceof \Shop\V1\Item);
if ($o->status !== \Shop\V1\Status::STATUS_PAID || $o->items[0]->blob !== 'hi' || $o->iban !== null) {
    fwrite(STDERR, "bad fields\n");
    exit(1);
}
echo json_encode($o->toArray());
`
	sp := filepath.Join(dir, "roundtrip.php")
	if err := os.WriteFile(sp, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(php, sp, dir, string(input)).CombinedOutput()
	if err != nil {
		t.Fatalf("php: %v: %s", err, out)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip:\n got %v\nwant %v", got, want)
	}
}
