package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cairophp/cairo/internal/core"
	"github.com/cairophp/cairo/internal/filewatch"
)

// How often --watch looks at the files.
const watchInterval = 250 * time.Millisecond

// reload gives the server the code that is on disk now, and reports
// whether it could not because server.php declares other servers, routes
// or settings: those need a new process.
func reload(engine *core.Engine, log *slog.Logger, because string) (declarationChanged bool) {
	start := time.Now()
	err := engine.Reload()
	switch {
	case err == nil:
		log.Info("php code reloaded", "because", because, "took", time.Since(start).Round(time.Millisecond).String())
	case errors.Is(err, core.ErrDeclarationChanged):
		return true
	case errors.Is(err, core.ErrRestartNeeded):
		// The engine has reported it: the server starts anew in place, and
		// has the new code then.
	default:
		log.Error("php code not reloaded", "because", because, "err", err)
	}
	return false
}

// reloadOnSignal reloads the code at each SIGUSR1, until the engine stops:
// how a deploy puts new code in a running server.
func reloadOnSignal(engine *core.Engine, log *slog.Logger) {
	asked := make(chan os.Signal, 1)
	signal.Notify(asked, syscall.SIGUSR1)
	go func() {
		defer signal.Stop(asked)
		for {
			select {
			case <-asked:
				if reload(engine, log, "signal") {
					log.Error("php code not reloaded", "because", "signal", "err", core.ErrDeclarationChanged)
				}
			case <-engine.Stopping():
				return
			}
		}
	}()
}

// watch reloads the code whenever a file under paths changes, until ctx
// ends. A change that only a new process can serve (another declaration, a
// .env file) is sent on the channel it returns, once.
func watch(ctx context.Context, engine *core.Engine, log *slog.Logger, paths []string) <-chan struct{} {
	restart := make(chan struct{}, 1)
	go func() {
		err := filewatch.Watch(ctx, paths, watchInterval, func(changed []string) {
			because := changed[0]
			if len(changed) > 1 {
				because += " and " + plural(len(changed)-1, "other file")
			}
			why := ""
			switch {
			case environment(changed):
				why = "the environment files changed, and what PHP has put in the process's environment stays there"
			case reload(engine, log, because):
				why = "server.php declares other servers, routes or settings"
			default:
				return
			}
			log.Info("restarting in place", "because", because, "why", why)
			select {
			case restart <- struct{}{}:
			default:
			}
		})
		if err != nil {
			log.Error("not watching for changes", "err", err)
		}
	}()
	return restart
}

// environment reports whether a .env file is among paths. A library that
// reads one (vlucas/phpdotenv, as Laravel uses it) puts its values in the
// process's environment with putenv() and leaves alone what is set there
// already: a thread that starts later would keep the old values.
func environment(paths []string) bool {
	for _, path := range paths {
		if name := filepath.Base(path); name == ".env" || strings.HasPrefix(name, ".env.") {
			return true
		}
	}
	return false
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return strconv.Itoa(n) + " " + what + "s"
}

// watchPaths is what --watch looks at: the paths that were named, or the
// directory of the server file, and always the server file itself.
func watchPaths(serverFile string, named []string) ([]string, error) {
	file, err := filepath.Abs(serverFile)
	if err != nil {
		return nil, err
	}
	if len(named) == 0 {
		return []string{filepath.Dir(file)}, nil
	}
	paths := []string{file}
	for _, path := range named {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		paths = append(paths, abs)
	}
	return paths, nil
}
