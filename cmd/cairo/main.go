// Command cairo runs PHP applications whose HTTP and gRPC servers are declared
// in PHP and served by Go.
//
//	cairo serve server.php [--threads N] [--offload-threads N]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mohamed-samir907/cairo/internal/grpcserver"
	"github.com/mohamed-samir907/cairo/internal/httpserver"
	"github.com/mohamed-samir907/cairo/internal/sapi"
)

const version = "0.1.0-dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "cairo:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  cairo serve <server.php> [--threads N] [--offload-threads N] [--grace 30s]
  cairo version`)
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("missing command")
	}
	switch args[0] {
	case "version":
		fmt.Println("cairo", version)
		return nil
	case "serve":
		return serve(args[1:])
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

type server interface {
	Addr() string
	Serve() error
	Shutdown(context.Context) error
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	threads := fs.Int("threads", 0, "PHP request threads (overrides server.php; default: one per CPU core)")
	offloadThreads := fs.Int("offload-threads", 0, "PHP threads for Cairo\\offload() jobs (overrides server.php; default: one per CPU core)")
	grace := fs.Duration("grace", 30*time.Second, "how long to wait for in-flight requests on shutdown")
	// Accept the server file before or after the flags.
	var file string
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		file, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if file == "" {
		file = fs.Arg(0)
	}
	if file == "" {
		file = "server.php"
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	engine, err := sapi.Start(sapi.Options{ServerFile: file, Threads: *threads, OffloadThreads: *offloadThreads, Logger: log})
	if err != nil {
		return err
	}

	var servers []server
	for _, decl := range engine.Spec().HTTP {
		s, err := httpserver.New(decl, engine, log)
		if err != nil {
			return fmt.Errorf("http %s: %w", decl.Listen, err)
		}
		servers = append(servers, s)
		log.Info("http server listening", "addr", s.Addr(), "routes", len(decl.Routes))
	}
	for _, decl := range engine.Spec().GRPC {
		s, err := grpcserver.New(decl, engine, log)
		if err != nil {
			return fmt.Errorf("grpc %s: %w", decl.Listen, err)
		}
		servers = append(servers, s)
		log.Info("grpc server listening", "addr", s.Addr(), "services", len(decl.Services))
	}

	errs := make(chan error, len(servers))
	for _, s := range servers {
		go func(s server) { errs <- s.Serve() }(s)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	var runErr error
	select {
	case got := <-sig:
		log.Info("shutting down", "signal", got.String())
	case runErr = <-errs:
	case runErr = <-engine.Errors():
	}

	ctx, cancel := context.WithTimeout(context.Background(), *grace)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(ctx)
	}
	if err := engine.Shutdown(ctx); err != nil {
		log.Warn("php shutdown", "err", err)
	}
	return runErr
}
