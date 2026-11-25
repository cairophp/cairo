package httpserver

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScriptsFind(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"index.php", "page.php", "sub/index.php", "sub/a.txt", "empty/a.txt", ".env", ".git/config.php", "noindex/page.PHP"} {
		path := filepath.Join(dir, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("<?php"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A symlink out of the directory is not followed.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "evil.php"), []byte("<?php"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "evil.php"), filepath.Join(dir, "evil.php")); err != nil {
		t.Fatal(err)
	}
	s, err := openScripts(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		path, name, pathInfo string
		redirect             bool
	}{
		{path: "/", name: "/index.php"},
		{path: "/page.php", name: "/page.php"},
		{path: "/page.php/a/b", name: "/page.php", pathInfo: "/a/b"},
		{path: "/page.php/", name: "/page.php"},
		{path: "/noindex/page.PHP", name: "/noindex/page.PHP"},
		{path: "/sub/", name: "/sub/index.php"},
		{path: "/sub", redirect: true},
		{path: "/empty/", name: "/index.php"},           // no index.php: the front controller
		{path: "/hello-world/", name: "/index.php"},     // a pretty link
		{path: "/missing.php", name: "/index.php"},      // not a file
		{path: "/.env", name: "/index.php"},             // hidden: never a file
		{path: "/.git/config.php", name: "/index.php"},  // hidden directory
		{path: "/sub/../page.php", name: "/page.php"},   // cleaned
		{path: "/../../etc/passwd", name: "/index.php"}, // cleaned to the root
		{path: "/evil.php", name: "/index.php"},         // a symlink out of the root
	} {
		sc, redirect := s.find(c.path)
		if redirect != c.redirect {
			t.Errorf("%s: redirect %v, want %v", c.path, redirect, c.redirect)
			continue
		}
		if c.redirect {
			continue
		}
		if sc == nil {
			t.Errorf("%s: no script, want %s", c.path, c.name)
			continue
		}
		if sc.name != c.name || sc.pathInfo != c.pathInfo {
			t.Errorf("%s: %s %q, want %s %q", c.path, sc.name, sc.pathInfo, c.name, c.pathInfo)
		}
		if want := filepath.Join(s.dir, filepath.FromSlash(c.name[1:])); sc.file != want {
			t.Errorf("%s: file %s, want %s", c.path, sc.file, want)
		}
	}
}

func TestScriptsWithoutFrontControllerAnswer404(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "page.php"), []byte("<?php"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := openScripts(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if sc, _ := s.find("/page.php"); sc == nil || sc.name != "/page.php" {
		t.Fatalf("/page.php: %+v", sc)
	}
	if sc, redirect := s.find("/other"); sc != nil || redirect {
		t.Fatalf("/other: %+v %v, want nothing (404)", sc, redirect)
	}
}

func TestScriptsNamedFrontController(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"app.php", "index.php", "sub/index.php"} {
		path := filepath.Join(dir, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("<?php"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s, err := openScripts(dir, "app.php")
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{"/pretty/link": "/app.php", "/index.php": "/index.php", "/sub/": "/sub/index.php", "/": "/index.php"} {
		if sc, _ := s.find(path); sc == nil || sc.name != want {
			t.Errorf("%s: %+v, want %s", path, sc, want)
		}
	}
	for _, bad := range []string{"../app.php", "sub/index.php", "app.txt", ".hidden.php"} {
		if _, err := openScripts(dir, bad); err == nil {
			t.Errorf("front controller %q accepted", bad)
		}
	}
}
