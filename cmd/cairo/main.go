// Command cairo runs PHP applications whose HTTP and gRPC servers are declared
// in PHP and served by Go.
//
//	cairo serve server.php [--threads N] [--offload-threads N]
//	cairo php artisan migrate
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cairophp/cairo/internal/codegen"
	"github.com/cairophp/cairo/internal/core"
	"github.com/cairophp/cairo/internal/grpcserver"
	"github.com/cairophp/cairo/internal/httpserver"
	_ "github.com/cairophp/cairo/internal/sapi" // the extensions: each joins the core from its init()
	"github.com/cairophp/cairo/internal/spec"
	"github.com/cairophp/cairo/internal/telemetry"
)

// version is set at build time: -ldflags "-X main.version=v0.1.0".
var version = "dev"

func main() {
	if args, ok := phpCommand(os.Args); ok {
		os.Exit(php(args))
	}
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "cairo:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  cairo serve <server.php> [--threads N] [--offload-threads N] [--grace 30s] [--drain 0s] [--php-ini FILE]
                           [--watch] [--watch-path PATH]...
  cairo php [options] <file.php> [args...]    PHP's command line (cairo php -h)
  cairo gen [--out DIR] [-I DIR]... <file.proto>...
  cairo version`)
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("missing command")
	}
	switch args[0] {
	case "help", "-h", "--help":
		usage()
		return nil
	case "version":
		fmt.Println("cairo", version)
		return nil
	case "serve":
		return serve(args[1:])
	case "gen":
		return gen(args[1:])
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// gen writes PHP message classes and service interfaces for .proto files.
func gen(args []string) error {
	fs := flag.NewFlagSet("gen", flag.ContinueOnError)
	out := fs.String("out", "gen", "output directory")
	var includes multiFlag
	fs.Var(&includes, "I", "import path for .proto imports (repeatable)")
	// Accept flags before and after the files.
	var protos, rest []string
	for _, a := range args {
		if strings.HasSuffix(a, ".proto") && !strings.HasPrefix(a, "-") {
			protos = append(protos, a)
		} else {
			rest = append(rest, a)
		}
	}
	if err := fs.Parse(rest); err != nil {
		return err
	}
	protos = append(protos, fs.Args()...)
	if len(protos) == 0 {
		usage()
		return errors.New("gen: no .proto files")
	}
	files, err := codegen.Generate(protos, codegen.Options{ImportPaths: includes})
	if err != nil {
		return err
	}
	if err := codegen.Write(*out, files); err != nil {
		return err
	}
	fmt.Printf("cairo gen: wrote %d files to %s (require %s/autoload.php)\n", len(files), *out, *out)
	return nil
}

type server interface {
	Addr() string
	Serve() error
	Shutdown(context.Context) error
}

// declarations is the servers that server.php declared.
type declarations struct {
	http []httpserver.Declaration
	grpc []grpcserver.Declaration
}

// read takes the servers out of what server.php declared and checks them.
// One Claims goes through all of them: no two servers, of any kind, share a
// listen address or a handler id.
func (d *declarations) read(s *spec.Spec) error {
	var claims spec.Claims
	var err error
	if d.http, err = httpserver.Declared(s, &claims); err != nil {
		return err
	}
	if d.grpc, err = grpcserver.Declared(s, &claims); err != nil {
		return err
	}
	if len(d.http)+len(d.grpc) == 0 {
		return errors.New("server.php declared no servers: call $app->http(...) or $app->grpc(...)")
	}
	return nil
}

// metrics reports whether a server declared a metrics endpoint.
func (d *declarations) metrics() bool {
	for _, h := range d.http {
		if h.MetricsPath != "" {
			return true
		}
	}
	return false
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	threads := fs.Int("threads", 0, "PHP request threads (overrides server.php; default: one per CPU core)")
	offloadThreads := fs.Int("offload-threads", 0, "PHP threads for Cairo\\offload() jobs (overrides server.php; default: one per CPU core)")
	grace := fs.Duration("grace", 30*time.Second, "how long to wait for in-flight requests on shutdown")
	drain := fs.Duration("drain", 0, "how long to keep serving after a stop signal while the readiness endpoint answers 503")
	phpIni := fs.String("php-ini", os.Getenv("CAIRO_PHP_INI"), "php.ini file or directory (default: $PHPRC, then PHP's own search; also CAIRO_PHP_INI)")
	watching := fs.Bool("watch", false, "reload the PHP code when a file changes (development)")
	var watched multiFlag
	fs.Var(&watched, "watch-path", "a directory or file for --watch to look at instead of the server file's directory (repeatable; implies --watch)")
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
	var decls declarations
	engine, err := core.Start(core.Options{ServerFile: file, Threads: *threads, OffloadThreads: *offloadThreads, PHPIni: *phpIni, Logger: log, Validate: decls.read})
	if err != nil {
		return err
	}

	tel, err := telemetry.Start(context.Background(), telemetry.Options{
		Spec: engine.Spec(), Metrics: decls.metrics(), Log: log,
		Threads: func() telemetry.Threads { return telemetry.Threads(engine.Stats()) },
	})
	if err != nil {
		return err
	}

	var servers []server
	for _, decl := range decls.http {
		s, err := httpserver.New(decl, engine, log, tel)
		if err != nil {
			return fmt.Errorf("http %s: %w", decl.Listen, err)
		}
		servers = append(servers, s)
		log.Info("http server listening", "addr", s.Addr(), "routes", len(decl.Routes), "tls", decl.TLS != nil)
	}
	for _, decl := range decls.grpc {
		s, err := grpcserver.New(decl, engine, log, tel)
		if err != nil {
			return fmt.Errorf("grpc %s: %w", decl.Listen, err)
		}
		servers = append(servers, s)
		log.Info("grpc server listening", "addr", s.Addr(), "services", len(decl.Services), "tls", decl.TLS != nil)
	}

	errs := make(chan error, len(servers))
	for _, s := range servers {
		go func(s server) { errs <- s.Serve() }(s)
	}

	reloadOnSignal(engine, log)
	var restart <-chan struct{}
	if *watching || len(watched) > 0 {
		paths, err := watchPaths(file, watched)
		if err != nil {
			return err
		}
		watchCtx, stopWatching := context.WithCancel(context.Background())
		defer stopWatching()
		restart = watch(watchCtx, engine, log, paths)
		log.Info("watching for changes", "paths", strings.Join(paths, ", "))
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	var runErr error
	restarting := false
	select {
	case got := <-sig:
		log.Info("shutting down", "signal", got.String())
		tel.Drain()
		keepServing(*drain, sig)
	case <-restart:
		restarting = true
	case runErr = <-errs:
	case runErr = <-engine.Errors():
		if errors.Is(runErr, core.ErrRestartNeeded) {
			log.Warn("restarting in place", "because", runErr)
			restarting, runErr = true, nil
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), *grace)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(ctx)
	}
	if err := engine.Shutdown(ctx); err != nil {
		log.Warn("php shutdown", "err", err)
	}
	// The last spans get a few seconds: a collector that is down must not
	// hold the process for the rest of the grace time.
	flush, cancelFlush := context.WithTimeout(ctx, 5*time.Second)
	defer cancelFlush()
	if err := tel.Shutdown(flush); err != nil {
		log.Warn("telemetry shutdown", "err", err)
	}
	if restarting {
		return restartProcess()
	}
	return runErr
}

// keepServing keeps the servers up for d after a stop signal, so that whatever
// sends them requests sees the readiness endpoint say no and stops first.
// A second signal ends the wait.
func keepServing(d time.Duration, sig <-chan os.Signal) {
	if d <= 0 {
		return
	}
	select {
	case <-time.After(d):
	case <-sig:
	}
}
