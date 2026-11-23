# Working on Cairo

Instructions for coding agents (and people) changing this repository. The
user documentation is in [cairophp/docs](https://github.com/cairophp/docs)
(the README is its front page); this file is about the code. The design doc
linked from the README explains why things are the way they are.

## What Cairo is

A PHP application server in one Go binary. `cairo serve server.php` runs the
PHP file to learn which HTTP and gRPC servers, routes and services it
declares; Go then does all the networking and hands each request to a PHP
thread, where it runs in its own Fiber. When PHP waits on I/O through Cairo,
only its fiber suspends: the thread's loop watches the sockets that PHP's own
functions wait on, and what Cairo does in Go waits there, on a goroutine that
the operation has for its own length.

## Map

| Path | What lives there |
|------|------------------|
| `cmd/cairo/` | The CLI: `serve`, `gen`, `version`. Its blank import of `internal/sapi` is what links the extensions in |
| `internal/core/` | The core engine. cgo: the embedded PHP (`cairo.c`, Cairo's own SAPI), PHP threads, the goroutines of Go operations, the offload pool, what fibers wait on and the op registry. What it exports is all that an extension can use, and it names no extension: `boundary_test.go` fails when it does |
| `internal/core/engine.go`, `dispatch.go`, `supervise.go` | The engine in Go: start and shutdown, a request's way to a PHP thread, thread supervision |
| `internal/core/operation.go`, `opworker.go`, `waitops.go`, `waitops.c`, `php/context.php` | What a fiber asks Go for. A fiber has no goroutine and no context in Go: `async()` and the derived contexts (`withTimeout()`, `withCancel()`) are PHP alone. An operation names its request and its scope (`Context::goScope()`: nothing, a deadline, or cancelable), `submit` runs it under the request's context, or under one made for it when it has a scope, on a goroutine that then waits for the thread's next operation, and the result is an event in the thread's inbox. `Context::cancel()` ends the operations in flight under it with `cairo_op_cancel()` |
| `internal/core/loop.c`, `loop.h`, `sockets.c`, `sockets.h`, `mailbox.go`, `php/waits.php` | The loop of a PHP thread and the waits that stay on it. `cairo_next_event()` is where the thread comes back between fibers: it wakes the parked fibers whose time is up and those whose sockets are ready, takes the events Go queued for it (its inbox) and, with none and no fiber to run, sleeps, in C and without a call into Go: on a futex, or in its epoll while a fiber waits on a socket; when it gave Go an operation or an answer on its way, it first watches the inbox for a few microseconds (`look`). The mailbox is the memory both sides read and write without a call: the inbox, how many events were queued and taken, and since when the thread runs PHP (0 while it sleeps), with when it last went on from one ready fiber to the next (`cairo_loop_resumes()`). Send to an inbox with `thread.send()`, which wakes the thread when it sleeps. `Waits` parks the fibers in `sleep()`, in `select()` and on a socket (`Waits::socket()`, behind the hooks of PHP's streams, PDO MySQL and curl) on the thread: it says until when and under which context, and C keeps the fiber and its timer (`parked.c`, below); no goroutine is involved, and a context that ends wakes them from PHP (`Waits::end()`). Each thread has an epoll of its own (`sockets.c`): one call arms a fiber's socket there and parks the fiber (`cairo_wait_socket()`), the loop wakes it once the socket is ready, without running PHP for it, and Go wakes a thread that sleeps in the epoll by writing to the thread's eventfd |
| `internal/core/php/scheduler.php`, `fibers.c`, `fibers.h`, `parked.c`, `parked.h` | The scheduler of a PHP thread. PHP decides and C keeps and switches. The loop in `scheduler.php` resumes the fibers that are ready (`cairo_run_ready()`), then takes events (`cairo_next_event()`); a request starts in a fiber, which answers it with the `Answer` of its server and ends it (`Scheduler::serve()`). `fibers.c` has the context each fiber runs under, in a reserved pointer of the fiber (`cairo_fiber_bind()`; `Context::current()` reads it), the fibers that wait for their next job instead of ending (`cairo_fiber_idle()`, `cairo_fiber_reuse()`; 64 at most) and the queue of those to resume. `parked.c` has the waits and their timers: a wait is a struct on the stack of the fiber that waits (`cairo_park()`; `cairo_park_fiber()` for a wait that begins in C), found by its id (`cairo_wake()`) and by its request (`cairo_parked_under()`). All of it is the memory of the thread's PHP request, so it counts against the thread's memory limits. PHP below 8.4 has no `zend_fiber_resume()` to call: there `fibers.c` switches with its own copy of it |
| `internal/core/queue.h`, `outbox.c`, `outbox.h`, `outbox.go` | The queues between Go and the PHP threads, and the way of an answer. A message is C memory that its sender pushes without a lock and its one receiver takes all at once (`queue.h`): a thread's inbox is one such queue, and the outbox is the one that all threads share for what they have for Go. `cairo_answer()` (declared in `extension.h`) queues the answer to a call there and the thread goes on, without running Go code; one goroutine (`collect`) takes the answers and hands each to the goroutine in `Dispatch` that waits for it, and learns there that a thread has come to its loop for the first time (`cairo_outbox_ready()`: a recycled thread serves until its replacement has) and of a thread that has ended. A body of 32 KiB or more is not copied for Go: its message leaves the list that `collect` frees and belongs to the call, `Engine.DispatchLent` returns the body as a view of it with a function that frees it (the HTTP server writes it to its client and releases it), and `Engine.Dispatch` copies it. The calls a thread holds and the contexts made for its operations are in tables under the thread's mutex, which that goroutine, dispatch and the thread's own calls into Go share |
| `internal/core/offload.c`, `offload.h`, `offload.go`, `php/offload.php` | The way of an `offload()` job, which Go has no part in. The request thread serializes the job and queues it straight into the inbox of the offload thread with the least to do (`cairo_offload_send()`), its fiber parks on the thread (`Jobs`, under its context like any wait there), and the offload thread queues the serialized result straight into the inbox of the request thread (`cairo_offload_done()`): two wake-ups of threads. Job ids are the process's, so a result that comes after its fiber stopped waiting, or for a thread that was replaced, finds nobody. Go names the offload threads at start and, for one that died in the middle of a job, tells the fiber that waits (`jobLost`) |
| `internal/core/goops.go` | The op registry: `core.RegisterOp("name", fn)` in Go, `Cairo\Internal\Go::call('name', $payload)` in PHP. The payload array goes to Go in `internal/wire`'s binary format, where `wire.Unmarshal` reads it into a struct by its `json` tags; results come back as events. `RegisterFiberOp` is for what PHP's hooked functions ask for |
| `internal/core/extension.go`, `extension.h`, `extension.c` | How an extension's C code joins the SAPI module: it defines a `cairo_extension` (what to run at module startup, when a PHP thread ends and when a thread that the extension interrupted reaches a safe point, the PHP functions it adds) and its Go file adds it with `core.RegisterC` from `init()`. C types do not cross Go packages, so `internal/sapi` has a `registerC` that takes its own |
| `internal/wire/` | The binary format of the events Go sends to PHP threads (requests, op results), which `internal/core/wire.c` decodes into PHP arrays, and of the arguments PHP passes to a Go operation, which `wire.c` encodes and `wire.Unmarshal` decodes. Bodies cross as raw bytes, and a value that something else reads or wrote crosses as it is (`wire.Raw`). An event may end in raw bytes that it leaves out (`wire.TailEvent`, the body of an HTTP request): the engine copies them behind the event, straight into the thread's message (`cairo_inbox_call()`) |
| `internal/core/php/` | The core's PHP files, one per concern, embedded in the binary. Every PHP thread runs `runtime.php`, which loads the core's other files (`app.php`, `context.php`, `channel.php`, `async.php`, `offload.php`, `waits.php`, `scheduler.php`, `go.php`; `request_local.php` on first use) and then the files of the extensions |
| `internal/core/phpfiles.go` | How an extension's PHP file joins the runtime: `core.RegisterPHP(files, "name.php")` from its Go file's `init()` (`registerPHP("name.php")` in `internal/sapi`), loaded at start, or on first use of the classes it names. The engine writes the files of the core and of every extension into one directory when it starts, so a file name can be taken once |
| `internal/core/onstart.go`, `probe.go` | What an extension runs when the engine starts (`RegisterStart`), and how it follows requests through the engine: `ProbeRequests` gives each request a `Probe`, which is told of the waits of its fibers and of the stretches of PHP its thread ran. The engine measures nothing itself. `internal/sapi/timing.go` is the one that asks: the request timing behind the slow log (`slowlog.go`) and the heavy routes (`heavy.go`) |
| `internal/sapi/` | The extensions that are cgo or Go operations: the runtime hooks, the PDO pgsql driver, the clients, the diagnostics, how an HTTP answer leaves PHP. Every file joins the core from its `init()` |
| `php/` | The PHP files of the extensions, one per concern, embedded in the binary |
| `internal/sapi/blocking.go`, `blocking.c`, `php/blocking.php` | The blocking detector, which watches threads instead of requests: it asks the engine which threads are running PHP and since when each runs the fiber it is in (`EachBusyThread`), and interrupts one that has for too long (`InterruptThread`), and that thread reports from the `interrupt` of the detector's `cairo_extension`. Its goroutine ends when the engine does (`Stopping`) |
| `php/http_answer.php`, `php/grpc_answer.php` | How a request of each server is answered. The scheduler knows no server: a server's PHP file gives it an `Answer` with `Scheduler::answerWith()`, which answers a request in the request's fiber and names its route for the logs. `internal/sapi/http_answer.c` has `cairo_respond_http()`, which sends an HTTP answer to Go (`cairo_answer()`). The form its status and headers cross in is the HTTP server's own: written there, read by `decodeAnswer` in `internal/httpserver/answer.go`. `internal/sapi/grpc_answer.c` has `cairo_respond_grpc()` for a gRPC call, read by `answer` in `internal/grpcserver/server.go`. A reply to a stream does not wait for the method to end: `cairo_grpc_send()` in `internal/sapi/grpc_stream.c` puts it in the stream's queue in Go with one call (`goGrpcSend` in `grpcsend.go`) |
| `internal/grpcserver/sendqueue.go`, `readahead.go` | The two ends of a gRPC stream as PHP has them, for a method it serves and for a call it makes as a client (`internal/sapi/grpcstream.go`). What PHP sends waits in a `SendQueue` for the one goroutine that writes: 64 messages at most, put there by a direct call from the PHP thread, or by a Go operation that waits when it is full. What the other side sends is read ahead of PHP by a `ReadAhead`, 64 messages or 1 MiB at most, and one `recv` operation takes all that has arrived |
| `php/leak_watch.php` | How a diagnostic joins the scheduler, which names none: `Scheduler::onStart()` runs it on each request thread that starts, and there it asks to be called after each request (`afterRequest()`) when the app turned it on. The detector itself (`leaks.php`) loads on first use |
| `php/cache.php`, `php/pool.php` | `Cairo\Cache` and `Cairo\Pool`, each loaded on first use |
| `internal/spec/` | The declaration that `server.php` sends to Go. This package reads the core's part (threads, the concurrency mode, limits) and keeps what each extension declared as PHP sent it, under the extension's name. `Claims` is what servers must not share: listen addresses and handler ids |
| `internal/httpserver/`, `internal/grpcserver/` | net/http and grpc-go servers built from the declaration. Each reads and checks its own part of it (`declaration.go`); `cmd/cairo` gives both to the engine to run in the declare phase |
| `internal/pbwire/` | A gRPC message between protobuf's bytes and the array PHP has for it, in `internal/wire`'s form, with no message object or JSON text in between. The array is protojson's, and whatever it does not convert exactly as protojson would (a google.protobuf type, an unusual value, every error) it leaves to `grpcserver/message.go`, which goes through protojson. Its tests compare both directions with protojson on random and damaged messages, and `test/grpc_kinds_test.go` has PHP compare with `json_decode()` |
| `internal/middleware/` | Go middleware declared from PHP |
| `internal/cachetag/` | Tags of cached responses: declared by routes in `httpserver`, purged from PHP through `sapi` |
| `internal/codegen/` | `cairo gen`: PHP classes and clients from `.proto` files |
| `stubs/` | Generated IDE stubs of the public API in `internal/core/php/` and `php/`; `scripts/publish-stubs.sh` copies this directory to `cairophp/stubs`, the Composer package, on each release |
| `examples/` | Runnable apps. The end-to-end tests serve these, so they are also fixtures |
| `test/` | End-to-end tests: they start the `cairo` binary on an example and talk to it over HTTP and gRPC |
| `scripts/` | `build-php.sh` (ZTS PHP into /opt/php-zts), `build-static.sh` (the static binary), `stubs.php`, `publish-stubs.sh`, `core-names.php` (what the core's PHP files name of the extensions, for the boundary test) |

## Core engine and extensions

Cairo is a core engine and extensions. The core is what every request
passes through, plus what needs the engine's private state: PHP threads,
fibers, the scheduler's queues and the stack switches. An extension is
anything `cairo serve` could boot and run a fiber without. By that rule the
HTTP server is an extension too: the first one, always compiled in.

Two questions place a piece of code. Would a fiber fail to run without it?
Does it reach into threads, fibers, the scheduler or the stack switches? A
yes to either makes it core.

| Side | What it holds |
|------|---------------|
| Core | The SAPI and the PHP threads, their supervision, dispatch (fibers and isolated mode), the fiber scheduler and request-local state, the bridge (wire events, the op registry, the answer path), waiting (the threads' epolls, parked fibers, timers), deadlines and the hard stop, the concurrency API (`Context`, `Channel`, `select()`, `async()`, `await()`, `sleep()`, `offload()`, `RequestLocal`), the declaration and boot |
| Extensions | Servers (HTTP, its response cache, middleware, gRPC), clients (`Grpc\Client`, `Cairo\Net`), databases (the PDO pgsql driver, `Cairo\Pool`), PHP hooks (streams, sleep, DNS, TLS, curl), bridges (PSR adapters), diagnostics (slow log, heavy routes, leaks, blocking), `cairo gen` |

The core is `internal/core` (Go and C) with its PHP files in
`internal/core/php`, and the two packages it imports, `internal/spec` and
`internal/wire`. Everything else is an extension: `internal/sapi` (those that
are cgo or Go operations) with their PHP files in `php/`, the servers in
`internal/httpserver` and `internal/grpcserver`, `internal/pbwire`,
`internal/middleware`, `internal/cachetag` and `internal/codegen`.

Extensions depend on the core, never the reverse: core code does not name an
extension. An extension reaches the core through what `internal/core` exports
and the scheduler's public methods: Go operations (`RegisterOp`,
`RegisterFiberOp`, with `CtxError` for a wait that the request's context
ended), a server that calls `Engine.Dispatch` (or `DispatchLent`, when it only writes the body on) and whose PHP file says how its
requests are answered (`Scheduler::answerWith()`, and `cairo_answer()` in
`extension.h` for the bytes of an answer), its own part of the declaration (`Engine.Spec()`,
`Spec.Section`), its own embedded PHP file (`RegisterPHP`), a
`cairo_extension` for its C code (`RegisterC`), a function that runs when the
engine starts (`RegisterStart`) and a channel that closes when it stops
(`Engine.Stopping`), a probe that follows each request through the engine
(`Engine.ProbeRequests`), the threads that are running PHP
(`Engine.EachBusyThread`), which it can interrupt (`Engine.InterruptThread`),
the engine's logger (`Logger`), and what a request thread runs when it starts
and after each request (`Scheduler::onStart()`, `afterRequest()`).

`Cairo\App` is the declaration's public face. `server.php` sees one class,
so it has a method for every setting, an extension's included, and it is
where PHP names the servers, as `cmd/cairo` is in Go. It does not look
inside what a server declared: that goes to Go under the extension's name,
and only the extension's Go package knows its shape. The settings of an
extension go the same way (`slowRequests()` and `heavyRoutes()` under
`timing`, `detectBlocking()` under `blocking`).

`internal/core/boundary_test.go` holds the line from the core's side. The
core's Go files import the standard library and each other. Its C code
includes its own headers besides PHP's and the system's. Its PHP files name
no class or function that an extension declares, except the three that
`Cairo\App` needs for the paragraph above (`Http\Server`, `Grpc\Server` and
`Hook`; `scripts/core-names.php` lists them). And `go test ./internal/core`
links the core with no extension, so it cannot use a symbol of one either.
What no test sees is a name inside a string, such as an operation's or a
file's: keep those out of the core in review.

A new feature goes on the extension side, in its own files. When it needs
something of the engine that the list above does not have, export a small
function from the core; do not move the feature in.

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
go test ./test ./internal/codegen ./internal/middleware ./internal/spec ./internal/wire -count=1
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
results. The frameworks tests run `composer install` and need the network.

`internal/core`, `internal/sapi` and `cmd/cairo` do not compile without the
PHP headers. `go vet ./...` only works where `make php` has run.

## Rules for changes

- **Keep generated files fresh.** After changing a PHP file of the runtime
  (`internal/core/php/` or `php/`), run `make stubs` (`TestStubsUpToDate` fails otherwise). After changing
  `internal/codegen`, regenerate `examples/greeter/gen` with
  `cairo gen --out examples/greeter/gen examples/greeter/proto/helloworld.proto`
  (`TestExampleIsFresh` fails otherwise).
- **A new PHP-facing feature has four parts:** the Go op or hook in its own
  file in `internal/sapi`, the PHP API with docblocks (they become the stubs;
  see Code standards for where it goes), an example route or script under `examples/`, and an
  end-to-end test in `test/` that serves that example. Document it on the
  page for that area in cairophp/docs, in a pull request linked from yours.
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
  `core.RegisterOp`, the test helpers `freeAddr` and `waitFor`, and the
  existing examples already solve most plumbing.
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

- **One file, one concern**, named after it: `pgshared.go`, `grpcstream.go`,
  `tls_unix.go`. A new capability gets a new file, never a new section at the
  bottom of a big one.
- **Existing oversized files** (`internal/sapi/pdo_pgsql.c`,
  `test/e2e_test.go`) must not grow by a whole feature. A new PHP subsystem goes in its own file
  under `php/`, added to the runtime with `registerPHP` from its Go file and,
  when it has public classes or functions, to the list in `scripts/stubs.php`
  (`php/cache.php` with `internal/sapi/cachepurge.go` is the model). New C goes in its own `.c` file with a
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

- **Never block the PHP thread.** Every wait suspends its fiber: through Go
  as an operation on a goroutine, or parked on the thread's own loop (`Waits`, for
  what needs no Go: a sleep, a channel, a socket). See Concurrency is the point. A new
  blocking call without a hook is a bug, even if tests pass.
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
- **No N+1 calls across the PHP–Go boundary.** Each `Go::call` crosses cgo,
  with its arguments encoded one way and an event the other; batch work into one op instead of calling per item in a loop.
- **Measure before optimising.** Claim a speed-up only with numbers
  (`go test -bench`, the 50-requests-on-one-thread tests, or wrk), and put
  them in the PR. The benchmark suite against other PHP servers and the log
  of every performance change live in
  [cairophp/benchmarks](https://github.com/cairophp/benchmarks): run it from
  a checkout next to this one, and record the change there in
  `IMPROVEMENTS.md`.

### Security

- **Never trust request input**: headers, bodies, query strings, gRPC
  metadata, proto fields, file names. Validate type, length and range at the
  edge, then pass typed values inward.
- **SQL only with parameters.** `$1`/`?` placeholders in PDO;
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
  context (the call in `dispatch.go`, a wrapped gRPC stream) is the exception.
  Respect cancellation in loops.
- **Goroutines** have an owner and an exit: every `go func()` ends when its
  context is cancelled or its work is done. No leaked goroutines (see
  the blocking detector's goroutine in `blocking.go` for the pattern).
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
- **Exceptions** are specific classes (`Grpc\Exception`, `NetException`),
  never bare `\Exception`; never swallow one silently.
- The PHP floor is 8.1: do not use newer syntax in `internal/core/php/`,
  `php/` or the examples unless guarded by a version check.

### C

- Add no compiler warnings, and match the existing style in
  `internal/core/*.c` and `internal/sapi/*.c`.
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
- [ ] CHANGELOG (Unreleased) is updated, and cairophp/docs has a linked pull
      request, if users see the change.

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
   with a hyphen (`v0.2.0-rc.1`) become prereleases. It then calls
   `.github/workflows/image.yml`, which puts those binaries on Alpine
   (`Dockerfile.image`) and pushes `ghcr.io/cairophp/cairo` tagged `x.y.z`,
   `x.y` and `latest`.
4. Publish the stubs under the same tag, by hand for now:
   `git clone git@github.com:cairophp/stubs.git /tmp/stubs && scripts/publish-stubs.sh /tmp/stubs vx.y.z`.

## Writing apps on Cairo

When you write or review PHP that runs on Cairo, rather than Cairo itself:

- Install the stubs (`cairophp/stubs` as a dev dependency) so the
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
