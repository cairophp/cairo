# Working on Cairo

Instructions for coding agents (and people) changing this repository. The
README is the user documentation; this file is about the code. The design doc
linked from the README explains why things are the way they are.

## What Cairo is

A PHP application server in one Go binary. `cairo serve server.php` runs the
PHP file to learn which HTTP and gRPC servers, routes and services it
declares; Go then does all the networking and hands each request to a PHP
thread, where it runs in its own Fiber with its own goroutine. When PHP waits
on I/O through Cairo, only its fiber suspends and Go does the waiting.

## Map

| Path | What lives there |
|------|------------------|
| `cmd/cairo/` | The CLI: `serve`, `gen`, `version` |
| `internal/sapi/` | cgo: the embedded PHP (`cairo.c`, Cairo's own SAPI), PHP threads, fiber goroutines, offload pool, runtime hooks, and every Go op PHP can call |
| `internal/sapi/goops.go` | The op registry: `registerOp("name", fn)` in Go, `Cairo\Internal\Go::call('name', $payload)` in PHP. Payloads and results cross as JSON |
| `php/runtime.php` | The whole PHP side: every `Cairo\` class and the fiber scheduler. Embedded in the binary |
| `php/revolt.php` | `Cairo\Revolt\Driver`, loaded when `revolt/event-loop` is installed |
| `internal/spec/` | The server declaration that `server.php` sends to Go |
| `internal/httpserver/`, `internal/grpcserver/` | net/http and grpc-go servers built from the declaration |
| `internal/middleware/` | Go middleware declared from PHP |
| `internal/codegen/` | `cairo gen`: PHP classes and clients from `.proto` files |
| `stubs/` | Generated IDE stubs of `php/runtime.php`, shipped as the Composer package |
| `examples/` | Runnable apps. The end-to-end tests serve these, so they are also fixtures |
| `test/` | End-to-end tests: they start the `cairo` binary on an example and talk to it over HTTP and gRPC |
| `scripts/` | `build-php.sh` (ZTS PHP into /opt/php-zts), `build-static.sh` (the static binary), `stubs.php` |

## Building and testing

Cairo is cgo against a thread-safe (ZTS) libphp with the embed SAPI. There
are two ways to get one; pick by what the machine has.

**Native** (needs a C toolchain, bison, re2c and the -dev packages listed in
`scripts/build-php.sh`; installs into /opt/php-zts):

```sh
make php        # once; compiles PHP
make build      # bin/cairo
make test       # go vet + all tests
```

**Docker only** (no system packages; this is what CI's static job does):

```sh
make static     # dist/cairo; about 17 minutes the first time, then ~20 s
docker run -d --name cairo-mysql -e MARIADB_ROOT_PASSWORD=cairo -p 3306:3306 mariadb:11.4
docker run -d --name cairo-pg -e POSTGRES_USER=cairo -e POSTGRES_PASSWORD=cairo -e POSTGRES_DB=cairo -p 5432:5432 postgres:16
CAIRO_TEST_BIN=$PWD/dist/cairo CAIRO_TEST_STATIC=1 CAIRO_TEST_NO_OPCACHE=1 \
CAIRO_TEST_MYSQL='mysql:host=127.0.0.1;port=3306' CAIRO_TEST_MYSQL_USER=root CAIRO_TEST_MYSQL_PASSWORD=cairo \
CAIRO_TEST_PGSQL='pgsql:host=127.0.0.1;port=5432;dbname=cairo' CAIRO_TEST_PGSQL_USER=cairo CAIRO_TEST_PGSQL_PASSWORD=cairo \
go test ./test ./internal/codegen ./internal/middleware ./internal/spec -count=1
```

`make static` builds from the working tree at the moment Docker copies it.
Do not switch branches while it runs; build from a `git archive` of the
branch into a temporary directory if you need to.

What the test environment variables do:

| Variable | Effect |
|----------|--------|
| `CAIRO_TEST_BIN` | Test this binary instead of building one (needs no local PHP) |
| `CAIRO_TEST_STATIC=1` | Also run `TestStaticBinary` (checks it is static) |
| `CAIRO_TEST_NO_OPCACHE=1` | Do not require opcache (the static binary has none before PHP 8.5) |
| `CAIRO_TEST_MYSQL`, `_USER`, `_PASSWORD` | PDO DSN for the MySQL tests; skipped when unset |
| `CAIRO_TEST_PGSQL`, `_USER`, `_PASSWORD` | PDO DSN for the PostgreSQL tests; skipped when unset |
| `PHP_BIN` | A PHP CLI for the tests that need one (stubs, codegen) |

Skipped tests are not passing tests: say which ones skipped when you report
results. The frameworks and Revolt tests run `composer install` and need the
network.

Packages under `internal/sapi` and `cmd/cairo` do not compile without the
PHP headers. `go vet ./...` only works where `make php` has run.

## Rules for changes

- **Keep generated files fresh.** After changing `php/runtime.php`, run
  `make stubs` (`TestStubsUpToDate` fails otherwise). After changing
  `internal/codegen`, regenerate `examples/greeter/gen` with
  `cairo gen --out examples/greeter/gen examples/greeter/proto/helloworld.proto`
  (`TestExampleIsFresh` fails otherwise).
- **A new PHP-facing feature has four parts:** the Go op or hook in
  `internal/sapi`, the PHP API in `php/runtime.php` with docblocks (they
  become the stubs), an example route or script under `examples/`, and an
  end-to-end test in `test/` that serves that example. Document it in the
  README section for that area.
- **Concurrency is the point.** A feature that waits must suspend only its
  fiber. Test that explicitly: the existing tests run 50 requests that each
  wait 1 s on ONE PHP thread and require them to finish in about 1 s. Copy
  that pattern.
- **Context everywhere.** Go ops receive the calling fiber's
  `context.Context`. Use it for every outgoing call so deadlines and
  cancellation reach the I/O.
- **Go memory stays in Go.** Long-lived Go values (connections, pools,
  streams) live in Go maps keyed by integer ids; PHP holds the id and frees it
  in a destructor or when the request ends.
- **Typed, curated APIs.** No generic "call any Go function" bridge. Each
  capability gets a deliberate `Cairo\...` API with PHP types and exceptions.
- **No results that depend on the machine.** Format times in UTC, sort map
  keys where output is compared, and do not assume a time zone or CPU count.
- **Style.** Match the surrounding code: Go is gofmt'ed with short comments
  that say why; PHP is `declare(strict_types=1)`, PSR-12, typed, with
  docblocks for arrays (`array<string, mixed>`, `list<string>`).
- **Licenses.** Cairo is Apache-2.0. Code adapted from elsewhere needs its
  license in `third_party/NOTICES`.

## Pull requests

- Branch from `main` and open the PR against `main`. Do not stack PRs on
  other feature branches: a PR merged into a branch that already merged never
  reaches `main`.
- Add a line under `## [Unreleased]` in `CHANGELOG.md`.
- Say in the PR how you tested: which binary (native or static), which
  databases, and which tests skipped.
- CI (`.github/workflows/ci.yml`) runs the suite on a native ZTS build and
  against the static binary on amd64 and arm64, with MariaDB and PostgreSQL.

## Releasing

1. Move the Unreleased entries of `CHANGELOG.md` into a new
   `## [x.y.z] - YYYY-MM-DD` section and update the links at the bottom.
2. Merge that to `main`, then tag it: `git tag vx.y.z && git push origin vx.y.z`.
3. `.github/workflows/release.yml` builds the static binaries for amd64 and
   arm64 with `cairo version` set to the tag, checks them, and publishes a
   GitHub release with `SHA256SUMS` and the changelog section as notes. Tags
   with a hyphen (`v0.2.0-rc.1`) become prereleases.

## Writing apps on Cairo

When you write or review PHP that runs on Cairo, rather than Cairo itself:

- Install the stubs (`mohamed-samir907/cairo` as a dev dependency) so the
  `Cairo\` API is visible to you and to PHPStan. Never `require` the stubs.
- Handlers are `fn (Request $req, Context $ctx): Response` for HTTP and the
  methods of the interface `cairo gen` writes for gRPC. Keep request state on
  `Request` and in `Context` values (`Context::set()`, `Context::get()`), never in globals or
  static properties: other requests run on the same thread at the same time.
- Use `Cairo\async()` and `await()` to do I/O in parallel, and `offload()`
  for CPU-heavy work. Waiting through PDO, curl, streams, `sleep()` or the
  `Cairo\` clients is fine; a C extension that opens its own sockets blocks
  the thread (turn on `$app->detectBlocking()` to find it).
- Code that is not fiber-safe (relies on `$_GET`, `$_SESSION`, or singletons
  holding the current request) goes behind `->concurrency('isolated')`.
- Throw `Cairo\Grpc\Exception` with a `Code` for gRPC errors; anything else
  becomes INTERNAL.
- `examples/` has a working app for each feature; start from the closest one.
