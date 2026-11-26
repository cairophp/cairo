package test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// With --watch, a changed script is the code of the next request, served
// either way, even when php.ini has opcache look at its files only now and
// then (testdata/classic_reload/php.ini).
func TestClassicWatchReloadsTheScripts(t *testing.T) {
	if os.Getenv("CAIRO_TEST_NO_OPCACHE") != "" {
		t.Skip("this binary has no opcache")
	}
	ini, _ := filepath.Abs("testdata/classic_reload/php.ini")
	server := "<?php\n\n$app = (new Cairo\\App())->concurrency('classic')->threads(1);\n" +
		"$app->http(getenv('CAIRO_HTTP'))->scripts(__DIR__ . '/site');\n\nreturn $app;\n"
	write := func(t *testing.T, name, code string) {
		if err := os.WriteFile(name, []byte(code), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, way := range []string{"declared", "plain"} {
		t.Run(way, func(t *testing.T) {
			dir := t.TempDir()
			site := filepath.Join(dir, "site")
			script := filepath.Join(site, "index.php")
			if err := os.MkdirAll(site, 0o755); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(dir, "server.php"), server)
			write(t, script, "<?php echo 'v1';")
			// opcache leaves a file of the last two seconds out of its
			// cache (opcache.file_update_protection): this one goes in.
			old := time.Now().Add(-time.Minute)
			if err := os.Chtimes(script, old, old); err != nil {
				t.Fatal(err)
			}
			addr := freeAddr()
			args := []string{filepath.Join(dir, "server.php")}
			if way == "plain" {
				args = []string{site, "--listen", addr, "--threads", "1"}
			}
			base := startScripts(t, addr, append(args, "--offload-threads", "1", "--watch", "--php-ini", ini), []string{"CAIRO_HTTP=" + addr})
			if _, body := classicGet(t, http.DefaultClient, base+"/"); body != "v1" {
				t.Fatalf("before the change: %q", body)
			}
			write(t, script, "<?php echo 'v2';")
			waitUntil(t, "the changed script answers", func() bool {
				_, body := classicGet(t, http.DefaultClient, base+"/")
				return strings.TrimSpace(body) == "v2"
			})
		})
	}
}
