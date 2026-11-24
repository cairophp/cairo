package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
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
// ends. A change that only a new process can serve is sent on the channel
// it returns, once.
func watch(ctx context.Context, engine *core.Engine, log *slog.Logger, paths []string) <-chan struct{} {
	restart := make(chan struct{}, 1)
	go func() {
		err := filewatch.Watch(ctx, paths, watchInterval, func(changed []string) {
			because := changed[0]
			if len(changed) > 1 {
				because += " and " + plural(len(changed)-1, "other file")
			}
			if reload(engine, log, because) {
				log.Info("restarting: server.php declares other servers, routes or settings", "because", because)
				select {
				case restart <- struct{}{}:
				default:
				}
			}
		})
		if err != nil {
			log.Error("not watching for changes", "err", err)
		}
	}()
	return restart
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

// restartProcess replaces this process with a new one of the same command,
// after the servers and PHP have stopped: the same process id, for whatever
// started it.
func restartProcess() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return syscall.Exec(exe, os.Args, os.Environ())
}
