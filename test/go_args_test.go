package test

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The arguments of a Go operation cross as PHP values, not as JSON text: a
// list fills a list, an array with keys or a plain object is a map, an
// object that says how it is encoded is asked, a string may hold any bytes,
// and what cannot cross is refused in PHP.
func TestGoOperationArguments(t *testing.T) {
	addr := freeAddr()
	cmd := exec.Command(cairoBin, "serve", "testdata/go-args/server.php", "--threads", "1", "--offload-threads", "1")
	cmd.Env = append(os.Environ(), "CAIRO_HTTP="+addr)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		_ = cmd.Wait()
	})
	if err := waitFor(addr, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get("http://" + addr + "/args")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var got map[string]string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	for name, want := range map[string]string{
		"list":              "ok",
		"empty":             "ok",
		"null":              "ok",
		"not UTF-8":         "ok",
		"enum":              "ok",
		"JsonSerializable":  "ok",
		"string keys":       "a map does not fit",
		"keys out of order": "a map does not fit",
		"stdClass":          "a map does not fit",
		"int":               "an integer does not fit",
		"float":             "a float does not fit",
		"bool":              "a bool does not fit",
		"string":            "a string does not fit",
		"resource":          "ValueError",
		"too deep":          "ValueError",
		"holds itself":      "ValueError",
	} {
		if !strings.Contains(got[name], want) {
			t.Errorf("%s: %q, want %q", name, got[name], want)
		}
	}
}
