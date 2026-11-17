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
CAIRO_TEST_BIN=$PWD/dist/cairo CAIRO_TEST_STATIC=1 \
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
| `CAIRO_TEST_NO_OPCACHE=1` | Do not require opcache (for a static binary built with PHP older than 8.5) |
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
- **A new PHP-facing feature has four parts:** the Go op or hook in its own
  file in `internal/sapi`, the PHP API with docblocks (they become the stubs;
  see Code standards for where it goes), an example route or script under `examples/`, and an
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

## Code standards

These apply to every change, whoever writes it. When a rule and the code
around it disagree, follow the rule in new code and do not rewrite old code
just to match, unless that is the task.

### Compare solutions before you pick one

For anything beyond a one-line fix, do not ship the first approach that
works. Challenge it:

1. **Find at least two real options** when they exist: for example a Go op
   versus a runtime hook, a channel versus a mutex, one batched `Go::call`
   versus one per item, the standard library versus a dependency, or
   changing the caller versus changing the API.
2. **Score each one** on the three things that matter here, in this order
   when they conflict:
   - **Security:** what can untrusted input reach? What fails open?
   - **Efficiency:** work per request, allocations, PHP–Go crossings, and
     whether it can block the PHP thread.
   - **Readability:** lines of code, new concepts, and how easy it is to
     delete later.
3. **Pick the best and apply it.** Prefer the simpler one when the scores
   are close. Choose a faster but more complex option only when a measurement
   shows the gain is worth it.
4. **Write the decision down** in the PR description: the options you
   considered, which one you chose, and why the others lost (one line each).
   For a hot path, include the numbers.

Do not invent options for show. If there is only one sensible way, say so in
one line and move on.

### Write less code

- **Solve the task in front of you.** No speculative options, extension
  points, interfaces with one implementation, or "for later" parameters. Add
  them when a second caller needs them.
- **Reuse before you write.** Search first (`grep -rn`): `Stream::call`,
  `sqlOp`, `registerOp`, the test helpers `freeAddr`, `waitFor` and
  `startSQLScript`, and the existing examples already solve most plumbing.
  Two copies of the same 10+ lines means extract a function; three means it
  was overdue.
- **Prefer the standard library.** A new Go module or Composer package needs a
  reason in the PR description (what it does that 50 lines of ours cannot).
  Check its license is compatible with Apache-2.0.
- **Delete what you replace.** No commented-out code, no dead branches, no
  "old" functions kept around. Git has the history.
- **Comments say why, not what.** One line on the reason, the invariant or the
  trap. If a comment restates the code, delete it; if code needs a paragraph
  to explain what it does, simplify the code.
- **Small diffs.** One concern per PR. A refactor and a feature are two PRs.

### No god files, no god functions

| Unit | Aim for | Hard limit: split before going past it |
|------|---------|----------------------------------------|
| Go file | < 400 lines | 600 lines |
| PHP file | < 400 lines | 600 lines |
| C file | < 800 lines | 1200 lines |
| Function or method | < 40 lines | 80 lines |
| Function parameters | ≤ 4 | 6 (then pass a struct or options array) |
| Nesting depth | ≤ 3 | 4 (return early instead) |

- **One file, one concern**, named after it: `sqldb.go`, `grpcstream.go`,
  `tls_unix.go`. A new capability gets a new file, never a new section at the
  bottom of a big one.
- **Existing oversized files** (`php/runtime.php`, `internal/sapi/cairo.c`,
  `internal/sapi/pdo_pgsql.c`, `internal/sapi/engine.go`, `test/e2e_test.go`)
  must not grow by a whole feature. A new PHP subsystem goes in its own file
  under `php/`, embedded in `php/embed.go` and loaded like `revolt.php`, and
  is added to `make stubs` (`scripts/stubs.php` reads one file today: teach it
  to read several in the same PR). New C goes in its own `.c` file with a
  header; new end-to-end tests go in their own `test/<feature>_test.go`.
  When a change touches a big file, moving the part you touch into its own
  file is welcome, as a separate commit.
- **No grab-bag files or packages:** no `utils.go`, `helpers.php`,
  `common/` or `misc`. Put a helper next to its only caller, or in a package
  named for what it does.
- **A function does one thing.** If its name needs "and", or it mixes
  parsing, I/O and formatting, split it.

### Efficiency

Cairo is a server: per-request code runs millions of times.

- **Never block the PHP thread.** Every wait goes through Go on the fiber's
  goroutine (see Concurrency is the point). A new blocking call without a
  hook is a bug, even if tests pass.
- **Nothing per request that can be done once.** Compile regexes, build
  lookup tables, parse config and resolve classes at boot or in the declare
  phase, not in the handler path.
- **Bound everything.** Every pool, queue, buffer, channel, map of ids and
  read from the network has a limit or a cleanup. Read bodies with a size cap
  (`io.LimitReader`, `maxBody`), give every outgoing call a deadline, and free
  id-keyed Go values when the request or fiber ends.
- **Allocate less on hot paths.** Preallocate slices when the length is known
  (`make([]T, 0, n)`), reuse buffers where profiling shows it matters, avoid
  `fmt.Sprintf` and reflection in per-request Go code, and avoid copying large
  PHP arrays (pass by reference only when measured).
- **No N+1 calls across the PHP–Go boundary.** Each `Go::call` crosses cgo
  and JSON; batch work into one op instead of calling per item in a loop.
- **Measure before optimising.** Claim a speed-up only with numbers
  (`go test -bench`, the 50-requests-on-one-thread tests, or wrk), and put
  them in the PR.

### Security

- **Never trust request input**: headers, bodies, query strings, gRPC
  metadata, proto fields, file names. Validate type, length and range at the
  edge, then pass typed values inward.
- **SQL only with parameters.** `$1`/`?` placeholders in `Cairo\Sql` and PDO;
  never build SQL with string concatenation or `sprintf`. Identifiers that
  must be dynamic come from an allow-list.
- **No shell, no eval.** No `exec.Command` with user input, no `sh -c`, no
  PHP `eval`, `unserialize` on untrusted data (use `json_decode`), `extract`,
  or variable variables. `offload()` task objects are serialized only between
  Cairo's own threads.
- **Paths.** Anything that maps a request to a file (`static()`, uploads)
  must `filepath.Clean` and confirm the result stays inside the root; reject
  `..`, absolute paths and symlinks out of the root.
- **TLS stays verified.** Never set `InsecureSkipVerify` or
  `verify => false` outside tests. Minimum TLS 1.2.
- **Secrets.** No credentials, tokens or keys in code, examples, tests or
  logs. Examples read them from the environment; logs redact DSN passwords
  and `Authorization` headers.
- **Errors to clients are generic.** Return a status and a short message;
  log the detail (stack, SQL, internal error) server side. Never send PHP
  stack traces or Go error chains in responses.
- **C and cgo.** Check every allocation and length; use `snprintf`/sized
  copies, never `strcpy`/`sprintf` into fixed buffers; free on every path;
  never keep a Go pointer in C memory past the call. Run new C under
  `-fsanitize=address` locally when it handles input.
- **Concurrency safety.** Shared Go state is guarded (mutex, `sync.Map` or
  atomics) and tests pass under `go test -race` where the package builds.
  PHP state shared by fibers (statics, globals) is a bug in fiber mode.
- **Dependencies.** Pin versions; check new ones with `govulncheck` (Go) and
  `composer audit` (PHP).

### Go

- `gofmt` and `go vet` clean. Follow Effective Go and the Go Code Review
  Comments.
- **Errors:** return them, wrap with context (`fmt.Errorf("open pool: %w",
  err)`), compare with `errors.Is`/`errors.As`. Handle each error once: log
  it or return it, not both. No `panic` for expected failures; panics are
  only for programmer errors, and request paths already recover.
- **Context** is the first parameter of anything that does I/O. Do not store
  it in long-lived structs; a per-request value that owns the request's
  context (the fiber in `engine.go`, a wrapped gRPC stream) is the exception.
  Respect cancellation in loops.
- **Goroutines** have an owner and an exit: every `go func()` ends when its
  context is cancelled or its work is done. No leaked goroutines (see
  `sqlBegin`'s cleanup goroutine for the pattern).
- **Interfaces** are small and defined where they are used, not where they
  are implemented. Accept interfaces, return concrete types.
- **Logging** with `log/slog`, structured key/value pairs, no `fmt.Println`
  in server code.
- **Tests** are table-driven where cases repeat, use `t.Helper()` and
  `t.Cleanup()`, and never `time.Sleep` to wait for a condition (poll with a
  deadline, as `waitFor` does).

### PHP

- `declare(strict_types=1);` at the top of every file. PSR-12 formatting,
  PSR-4 naming for classes.
- **Types everywhere**: parameter, return and property types; `readonly`
  properties and `final` classes by default; enums instead of string or int
  constants for closed sets. Arrays get a docblock shape
  (`array<string, mixed>`, `list<Row>`, `array{id: int, name: string}`).
- **PHPStan level 6 clean** for new code against the stubs; do not add
  `@phpstan-ignore` to hide a real type problem.
- **Fiber-safe by default:** no request state in static properties, globals
  or singletons. Use `Request`, `Context` values and constructor arguments.
- **Exceptions** are specific classes (`SqlException`, `Grpc\Exception`),
  never bare `\Exception`; never swallow one silently.
- The PHP floor is 8.1: do not use newer syntax in `php/runtime.php` or the
  examples unless guarded by a version check.

### C

- Add no compiler warnings, and match the existing style in
  `internal/sapi/*.c`.
- Use Zend's allocators (`emalloc`/`efree` for request memory,
  `pemalloc` for persistent) and `zend_string` APIs for PHP strings.
- Every function that can fail returns a status the caller checks; no
  silent truncation.
- ZTS rules: no mutable C globals shared between PHP threads without a lock.
  Per-thread state is `static __thread` (as `cairo_tid` is) or lives in the
  existing per-thread structures.
- Adapted code keeps its origin comment and license in `third_party/NOTICES`.

### Shell and CI

- Scripts start with `set -euo pipefail`, quote every variable, and use
  `"$@"` not `$*`. Check them with `shellcheck`.
- Workflows pin actions to a major version, request the least
  `permissions:` they need, and never echo secrets.

### Before you open the PR

- [ ] The diff does one thing, and nothing in it is unused.
- [ ] You compared the real alternatives and the PR says which won and why.
- [ ] No file passed its size limit; no new file is a grab-bag.
- [ ] New waits suspend only the fiber, with a test that proves it.
- [ ] Input is validated, SQL is parameterised, paths are confined, no
      secrets.
- [ ] `gofmt`, `go vet`, stubs and generated code are fresh; PHPStan is clean
      on new PHP.
- [ ] Tests cover the change, and you listed which tests ran and which
      skipped.
- [ ] README and CHANGELOG (Unreleased) are updated if users see the change.

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
