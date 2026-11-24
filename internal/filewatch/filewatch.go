// Package filewatch tells when the code of an app changes on disk, by
// looking: it reads the times and sizes of the files again and compares.
// The kernel's file events would be cheaper, but they do not arrive for a
// directory shared with a container or mounted over a network, which is
// where a development server often runs, and they need a module to use
// (github.com/fsnotify/fsnotify) and a watch on every directory.
package filewatch

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// maxFiles bounds a snapshot: more files than this is a path that was not
// meant (a home directory, a disk).
const maxFiles = 200_000

// settle is how long the files must stand still before a change is
// reported: a checkout or an editor writes more than one file.
const settle = 100 * time.Millisecond

type stamp struct {
	modified int64
	size     int64
}

// Snapshot is the watched files at one moment, by path.
type Snapshot map[string]stamp

// Scan reads the files under roots. A root that is a file is watched
// whatever its name. Under a root that is a directory, the watched files
// are PHP's, the templates and settings that a framework reads once and
// keeps (*.twig, *.yaml, *.yml) and the ones a PHP app is configured by
// (.env, .env.*, composer.lock), and what is not the app's own code is left out:
// directories whose name begins with a dot, node_modules, vendor (whose
// changes composer.lock shows), and storage and var, where frameworks write
// while they run.
func Scan(roots []string) (Snapshot, error) {
	snap := Snapshot{}
	for _, root := range roots {
		// WalkDir does not follow a link, the root included.
		real, err := filepath.EvalSymlinks(root)
		if err != nil {
			return nil, err
		}
		explicit := true
		err = filepath.WalkDir(real, func(path string, d fs.DirEntry, err error) error {
			top := explicit
			explicit = false
			if err != nil {
				if top {
					return err
				}
				return nil // a directory that went away or may not be read
			}
			if d.IsDir() {
				if !top && skipped(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			if !top && !watched(d.Name()) {
				return nil
			}
			info, err := os.Stat(path) // a link to a file counts as the file
			if err != nil || !info.Mode().IsRegular() {
				return nil
			}
			if len(snap) >= maxFiles {
				return fmt.Errorf("more than %d files to watch under %s: name the directories of the code", maxFiles, root)
			}
			snap[path] = stamp{info.ModTime().UnixNano(), info.Size()}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return snap, nil
}

func watched(name string) bool {
	switch filepath.Ext(name) {
	case ".php", ".twig", ".yaml", ".yml":
		return true
	}
	return name == ".env" || strings.HasPrefix(name, ".env.") || name == "composer.lock"
}

func skipped(dir string) bool {
	switch dir {
	case "node_modules", "vendor", "storage", "var":
		return true
	}
	return strings.HasPrefix(dir, ".")
}

// Diff returns the files that are new, gone or changed since prev, sorted.
func (s Snapshot) Diff(prev Snapshot) []string {
	var paths []string
	for path, now := range s {
		if before, ok := prev[path]; !ok || before != now {
			paths = append(paths, path)
		}
	}
	for path := range prev {
		if _, ok := s[path]; !ok {
			paths = append(paths, path)
		}
	}
	slices.Sort(paths)
	return paths
}

// Watch scans roots every interval until ctx ends, and calls changed with
// the files that changed once they have stood still for a moment. It
// returns at once with the error of the first scan, when that fails; a
// later scan that fails (a directory that is being replaced) is tried again.
func Watch(ctx context.Context, roots []string, interval time.Duration, changed func(paths []string)) error {
	seen, err := Scan(roots)
	if err != nil {
		return err
	}
	for wait(ctx, interval) {
		now, err := Scan(roots)
		if err != nil || len(now.Diff(seen)) == 0 {
			continue
		}
		for wait(ctx, settle) {
			next, err := Scan(roots)
			if err == nil && len(next.Diff(now)) == 0 {
				break
			}
			if err == nil {
				now = next
			}
		}
		if ctx.Err() != nil {
			break
		}
		changed(now.Diff(seen))
		seen = now
	}
	return nil
}

// wait reports false when ctx ended before d had passed.
func wait(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
