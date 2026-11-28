package filewatch

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// write puts a file of size bytes under dir, changed at second sec.
func write(t *testing.T, dir, name string, size int, sec int64) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	at := time.Unix(sec, 0)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
	return path
}

func scan(t *testing.T, roots ...string) Snapshot {
	t.Helper()
	snap, err := Scan(roots)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestScanTakesTheCodeOfTheApp(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		write(t, dir, ".env", 1, 1),
		write(t, dir, ".env.local", 1, 1),
		write(t, dir, "app/Http/Home.php", 1, 1),
		write(t, dir, "composer.lock", 1, 1),
		write(t, dir, "config/services.yaml", 1, 1),
		write(t, dir, "server.php", 1, 1),
		write(t, dir, "templates/home.html.twig", 1, 1),
	}
	for _, other := range []string{
		"readme.md", "app/style.css", "vendor/autoload.php", "node_modules/x/y.php",
		".git/hooks/pre-commit.php", "storage/framework/views/1.php", "var/cache/dev/Kernel.php",
	} {
		write(t, dir, other, 1, 1)
	}
	if got := scan(t, dir).Diff(nil); !slices.Equal(got, want) {
		t.Errorf("watched files:\n got %v\nwant %v", got, want)
	}

	// What is named is watched, whatever it is called and wherever it is.
	named := []string{filepath.Join(dir, "vendor"), filepath.Join(dir, "readme.md")}
	want = []string{filepath.Join(dir, "readme.md"), filepath.Join(dir, "vendor/autoload.php")}
	if got := scan(t, named...).Diff(nil); !slices.Equal(got, want) {
		t.Errorf("named roots:\n got %v\nwant %v", got, want)
	}

	// A root that is a link is the directory it names.
	link := filepath.Join(t.TempDir(), "current")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if got := len(scan(t, link)); got != 7 {
		t.Errorf("through a link: %d files, want 7", got)
	}
	if _, err := Scan([]string{filepath.Join(dir, "nowhere")}); err == nil {
		t.Error("a root that does not exist: no error")
	}
}

func TestDiffNamesWhatChanged(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	same := write(t, dir, "same.php", 1, 1)
	later := write(t, dir, "later.php", 1, 1)
	longer := write(t, dir, "longer.php", 1, 1)
	gone := write(t, dir, "gone.php", 1, 1)
	before := scan(t, dir)

	write(t, dir, "later.php", 1, 2)
	write(t, dir, "longer.php", 2, 1)
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	added := write(t, dir, "added.php", 1, 1)
	want := []string{added, gone, later, longer}
	if got := scan(t, dir).Diff(before); !slices.Equal(got, want) {
		t.Errorf("changed:\n got %v\nwant %v (and not %s)", got, want, same)
	}
}

func TestWatchReportsAChangeOnce(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	file := write(t, dir, "server.php", 1, 1)
	seen := scan(t, dir)
	// Written before Watch runs, after the look it is given: still a change.
	write(t, dir, "server.php", 2, 2)
	ctx, cancel := context.WithCancel(context.Background())
	changes := make(chan []string, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		Watch(ctx, []string{dir}, seen, 5*time.Millisecond, func(paths []string) { changes <- paths })
	}()

	var got []string
	select {
	case got = <-changes:
	case <-time.After(time.Second):
		t.Fatal("the change was not reported")
	}
	if !slices.Equal(got, []string{file}) {
		t.Errorf("changed = %v, want %v", got, []string{file})
	}
	select {
	case again := <-changes:
		t.Errorf("reported again: %v", again)
	case <-time.After(3 * settle):
	}
	cancel()
	<-done
}
