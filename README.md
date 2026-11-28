# Cairo

Cairo is a PHP application server written in Go. You declare HTTP and gRPC
servers in PHP; Go does the serving. Every request runs in its own PHP Fiber,
and a fiber that waits on I/O parks while its thread runs the others, so one
PHP thread can hold thousands of requests at once.

> Status: v0.8.1. The PHP API may still change before 1.0:
> [Stability](https://github.com/cairophp/docs/blob/main/stability.md) says what a release may change, and
> [CHANGELOG.md](CHANGELOG.md) lists what changed. Design doc:
> [Cairo: architecture and design proposal](https://claude.ai/artifact/PBihYMjib8P4eh56BboTPa)

```php
<?php // server.php
use Cairo\App;
use Cairo\Http\{Request, Response};
use function Cairo\{sleep, offload};

$app = new App();
$app->threads('auto');          // request threads; each runs many fibers
$app->offloadThreads('auto');   // CPU threads for Cairo\offload()

$http = $app->http(':8080');
$http->get('/hello/{name}', fn (Request $r) => Response::text("Hello {$r->param('name')}\n"));
$http->get('/slow', function (): Response {
    sleep(1);                    // only this fiber suspends
    return Response::json(['ok' => true]);
});
$http->get('/report', fn () => Response::json(offload('buildReport', 2026)));  // CPU work, other threads

$grpc = $app->grpc(':9000')->proto(__DIR__ . '/proto/helloworld.proto');
$grpc->service('helloworld.Greeter', Greeter::class);

return $app;
```

```sh
cairo serve server.php
cairo serve server.php --watch   # development: the new code is live when a file changes
```

The same binary is PHP's command line, so an image needs no second PHP for
`artisan`, `bin/console` or Composer
([PHP command line](https://github.com/cairophp/docs/blob/main/cli.md)):

```sh
cairo php artisan migrate
```

## Install

Cairo ships as one static binary for Linux on amd64 and arm64, with PHP 8.5,
opcache and the extensions built in. Nothing else is needed at run time, not even
libc. This installs the latest release into `/usr/local/bin`, after checking
it against the release's `SHA256SUMS`:

```sh
curl -fsSL https://raw.githubusercontent.com/cairophp/cairo/main/install.sh | bash
```

Or take the binary from the [releases page](https://github.com/cairophp/cairo/releases)
yourself. In a container, and on macOS and Windows through Docker, use the
[official image](https://github.com/cairophp/docs/blob/main/docker.md):

```dockerfile
FROM ghcr.io/cairophp/cairo:0.5
COPY --chown=cairo:cairo . /app
```

```sh
docker build -t my-app . && docker run -p 8080:8080 my-app   # runs: cairo serve server.php
```

It runs on Linux (amd64 and arm64); see
[supported platforms and known limitations](https://github.com/cairophp/docs/blob/main/install.md). To build it
yourself, see [Building](https://github.com/cairophp/docs/blob/main/building.md). For your IDE and PHPStan, add the
[stubs package](https://github.com/cairophp/docs/blob/main/ide.md).

## Documentation

The user documentation is in [cairophp/docs](https://github.com/cairophp/docs):

- **Start:** [install](https://github.com/cairophp/docs/blob/main/install.md), [Docker](https://github.com/cairophp/docs/blob/main/docker.md),
  [PHP command line](https://github.com/cairophp/docs/blob/main/cli.md),
  [how it works](https://github.com/cairophp/docs/blob/main/how-it-works.md), [stability](https://github.com/cairophp/docs/blob/main/stability.md)
- **Serving:** [gRPC](https://github.com/cairophp/docs/blob/main/grpc.md), [middleware](https://github.com/cairophp/docs/blob/main/middleware.md),
  [response cache](https://github.com/cairophp/docs/blob/main/response-cache.md), [TLS](https://github.com/cairophp/docs/blob/main/tls.md)
- **Concurrency:** [context and deadlines](https://github.com/cairophp/docs/blob/main/context.md),
  [async, await and offload](https://github.com/cairophp/docs/blob/main/concurrency.md),
  [request state](https://github.com/cairophp/docs/blob/main/context-values.md), [runtime hooks](https://github.com/cairophp/docs/blob/main/runtime-hooks.md),
  [finding blocking code](https://github.com/cairophp/docs/blob/main/blocking.md)
- **Data and I/O:** [PostgreSQL](https://github.com/cairophp/docs/blob/main/postgresql.md),
  [connection pools](https://github.com/cairophp/docs/blob/main/pools.md), [`Cairo\Net`](https://github.com/cairophp/docs/blob/main/net.md)
- **Running applications:** [isolated mode](https://github.com/cairophp/docs/blob/main/isolated-mode.md),
  [PSR-15, Symfony and Laravel](https://github.com/cairophp/docs/blob/main/frameworks.md),
  [thread supervision](https://github.com/cairophp/docs/blob/main/supervision.md),
  [metrics, health probes and tracing](https://github.com/cairophp/docs/blob/main/observability.md)
- **Tooling:** [IDE and static analysis](https://github.com/cairophp/docs/blob/main/ide.md),
  [building Cairo and PHP settings](https://github.com/cairophp/docs/blob/main/building.md)

`examples/` has a runnable app for each feature.

## Building

```sh
make static         # dist/cairo: one static binary, built in Docker
make php            # or natively: a ZTS PHP into /opt/php-zts, once
make build          # bin/cairo
make test
```

[Building](https://github.com/cairophp/docs/blob/main/building.md) has the details, the extensions and the PHP
settings; [AGENTS.md](AGENTS.md) has the test setup.

## License

Cairo is licensed under the [Apache License 2.0](LICENSE).

Cairo's SAPI is adapted from PHP's embed SAPI (PHP License 3.01) and follows
FrankenPHP (MIT) in places; `third_party/NOTICES` has both licenses. The static
binary also links PHP, its extensions and their libraries, under their own
licenses.

**The binary contains the [Mercure hub](https://github.com/dunglas/mercure),
which is AGPL-3.0, not Apache-2.0.** It serves `$http->mercure()`. That
makes a `cairo` binary built in the default way, the releases and the
official image among them, a combined work that carries the AGPL-3.0's
duties: `third_party/NOTICES` says what that means. `make build
TAGS=nomercure` builds a binary without the hub and without any AGPL code.

## Contributing

[AGENTS.md](AGENTS.md) describes how to build, test and change Cairo, for
people and coding agents alike. Changes go through pull requests against
`main`; add a line to `CHANGELOG.md` under Unreleased. To report a security
problem, see [SECURITY.md](SECURITY.md).

Benchmarks against PHP-FPM, FrankenPHP, RoadRunner and Swoole, and the log of
every performance change with its measured effect, are in
[cairophp/benchmarks](https://github.com/cairophp/benchmarks).

## Layout

```
cmd/cairo/            the binary (serve, php, gen)
internal/codegen/     cairo gen: PHP classes and clients from .proto files
internal/core/        the core engine: embedded PHP (C bridge), PHP threads, the goroutines of Go operations, offload pool
internal/core/php/    the core's PHP side (embedded in the binary); runtime.php is what every PHP thread runs
internal/sapi/        the extensions next to the embedded PHP: runtime hooks, PDO pgsql, clients, diagnostics
internal/spec/        the server declaration sent by server.php
internal/httpserver/  net/http server built from the declaration
internal/middleware/  Go middleware declared from PHP (HTTP and gRPC)
internal/cachetag/    tags of cached responses, for Cairo\Cache::purge()
internal/telemetry/   metrics (Prometheus), traces (OpenTelemetry) and readiness, as server.php declared them
internal/grpcserver/  grpc-go server, proto compilation, health, reflection
internal/filewatch/   tells cairo serve --watch when the code of the app changes
php/                  the PHP side of the extensions, one file per concern (embedded in the binary)
php/cache.php         Cairo\Cache, loaded on first use
php/pool.php          Cairo\Pool, loaded on first use
examples/greeter/     HTTP + gRPC example
examples/isolated/    isolated mode: superglobals, echo/header(), resets
examples/frameworks/  PSR-15 handlers and Symfony-style kernels (composer install first)
examples/supervise/   thread restart after a fatal error, maxRequests and maxMemory
examples/blocking/    detectBlocking: logs requests that hold their PHP thread
examples/cache/       response cache: ->cache() and Cache::purge()
examples/pool/        Cairo\Pool: wait timeout, reset, lifetimes
examples/mercure/     a Mercure hub in the server: $http->mercure() and mercure_publish()
examples/reload/      new code in a running server: cairo serve --watch and kill -USR1

stubs/                Cairo's PHP API as stubs for IDEs (generated by scripts/stubs.php)
test/                 end-to-end tests
third_party/NOTICES   licenses of code Cairo adapts (PHP embed SAPI, FrankenPHP) and links in (the Mercure hub, AGPL-3.0)
AGENTS.md             how to build, test and change Cairo (CLAUDE.md points here)
install.sh            installs the release binary for this machine
Dockerfile.image      the official image, built from a release's binaries
SECURITY.md           how to report a vulnerability
CHANGELOG.md          what changed in each release
LICENSE, NOTICE       Apache-2.0
```
