# Changelog

All notable changes to Cairo. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/). Before 1.0, a minor version may
change the PHP API; patch versions do not.

## [Unreleased]

### Added

- `Cairo\RequestLocal::register($class, ...$properties)`: static
  properties that keep one value per request, swapped in C when fibers of
  different requests take turns on a thread. The first step to running
  Laravel without isolated mode.
- `$http->publicDir($dir)`: serve a document root (Laravel's `public/`) from
  Go, with every path that is not a file going to the routes. PHP files and
  hidden files are never served.
- `App::detectLeaks()`: a development aid that logs the static properties a
  request changed but that are not request-local, so one request could read
  another's value. Caches that only gain keys are not reported.

### Changed

- Isolated mode: requests wait in Go in one first-in, first-out queue and go
  to the next free thread, instead of queuing on a thread behind its slow
  requests. Laravel (Octane) p99 at 256 connections: 2.1 s to 0.6 s.
- Internal, no change in behaviour: the Go engine is split by concern
  (`engine.go` into `dispatch.go`, `fiber.go`, `offload.go` and more), and the
  operations of the PHP hooks register through the op registry instead of
  being cases in the engine. First step of separating the core engine from
  the extensions; `AGENTS.md` describes the line.
- Internal, no change in behaviour: the runtime hooks leave `cairo.c` for
  `function_hook.c` and `socket_hook.c`, and the C code of the extensions
  (hooks, PDO pgsql, the query watcher, curl) registers with the SAPI module
  instead of being started by name. Second step of the same separation.
- Internal, no change in behaviour: `php/runtime.php` is split into one file
  per concern, and it loads the PHP files of the extensions from a list that
  each extension adds itself to (`registerPHP`) instead of naming them. Third
  step of the same separation.
- Internal, no change in behaviour: the fiber scheduler no longer knows HTTP
  or gRPC. Each server's PHP file registers what answers its requests
  (`php/http_answer.php`, `php/grpc_answer.php`), and `cairo_respond_http()`
  leaves `cairo.c` for `http_answer.c`. Fourth step of the same separation.
- Internal, no change in behaviour: `internal/spec` reads only the core's
  part of what `server.php` declares. The HTTP and gRPC servers read and
  check their own (`internal/httpserver/declaration.go`,
  `internal/grpcserver/declaration.go`), which `Cairo\App` sends under each
  one's name. Fifth step of the same separation.
- Internal, no change in behaviour: the engine no longer times requests for
  the slow log and the heavy routes. It tells a probe what a request waited
  for and how long its thread ran PHP (`internal/sapi/probe.go`), and the
  request timing asks for one when either report is on, with its settings in
  its own part of the declaration. Sixth step of the same separation.
- Internal, no change in behaviour: the blocking detector leaves the engine
  and `cairo.c` for its own files (`internal/sapi/blocking.go`, `blocking.c`,
  `php/blocking.php`). It asks the engine which threads are running PHP and
  interrupts one through a hook that the C code of any extension can use,
  with its setting in its own part of the declaration. Seventh step of the
  same separation.
- Internal, no change in behaviour: the fiber scheduler no longer starts the
  repeated query detector and the leak detector or knows how a server names
  its routes. An extension's PHP file hooks into each request thread
  (`Scheduler::onStart()`, `afterRequest()`), and what answers a server's
  requests also names their routes. Eighth step of the same separation.
- Internal, no change in behaviour: the form in which the status and headers
  of an HTTP answer cross from PHP to Go leaves the core's wire files
  (`internal/sapi/wire.c`, `internal/wire`) for the HTTP server's
  (`internal/sapi/http_answer.c`, `internal/httpserver/answer.go`). Ninth
  step of the same separation.
- Internal, no change in behaviour: the core engine has its own Go package,
  `internal/core`, with its PHP files in `internal/core/php`. The extensions
  stay in `internal/sapi` and `php/` and use only what the core exports
  (`core.RegisterOp`, `RegisterPHP`, `RegisterC` and the rest; `AGENTS.md`
  has the list), and a test fails when the core names an extension in Go, C
  or PHP (`internal/core/boundary_test.go`). Tenth and last step of the same
  separation.
- Performance: Cairo's own C code is compiled with `-O2`. The build scripts
  set `CGO_CFLAGS` to PHP's include paths, which dropped the optimization
  that Go asks for by default, so every build had compiled that code
  without any. A Go operation or a wait among 64 fibers is about 8% cheaper,
  and the PostgreSQL benchmark route serves 3.6% more requests per second.
- Performance: opcache holds Cairo's own PHP files. The engine writes them
  when the server starts, and opcache leaves out a file that changed in the
  last two seconds, so every PHP thread compiled the runtime itself, with no
  optimizer and nothing shared between threads. The files now get an old
  modification time.
- Performance: on glibc builds, a call from a PHP fiber into Go no longer
  makes Go's runtime fetch the thread's stack from the system each time (a
  `sched_getaffinity` system call and an allocation). Cairo reads each
  thread's stack once and answers from that.
- Performance: the engine keeps the calls a PHP thread took and the fibers
  that live on it in tables of that thread, which only the thread itself
  uses, in place of two maps shared and locked by all threads.
- Performance: a request's bookkeeping in Go (its call, its root fiber,
  their channels and the buffer of its event) is reused from one request to
  the next instead of allocated for each: eight allocations and about 1 kB
  less per request.
- Performance: a request's fibers wait under the context of the server's
  request itself. The engine made a context of its own under it for every
  request and ended it when PHP answered, which the server's context does
  anyway once the request is done: five allocations less per request.
- Performance: a request goes to a PHP thread that is already running PHP
  and will be back for its next event within 50 µs, when there is one,
  instead of waking a thread that sleeps. A thread that holds 32 more
  requests than the least busy one, or that has run one fiber for long, is
  still passed over.
- Performance: the arguments of a Go operation (`Cairo\Sql`, the HTTP, gRPC
  and network clients, the cache purge) cross from PHP to Go in the binary
  format that results already come back in, instead of as JSON text: no
  `json_encode()` in PHP, and Go reads them about three times as fast. An
  operation that runs on the PHP thread gets its result back the same way. A
  string argument may now hold any bytes, where JSON refused text that was
  not UTF-8.
- Performance: a fiber in `Cairo\sleep()`, in a hooked `sleep()` or
  `usleep()`, or blocked on a `Cairo\Channel`, in `select()` or on a
  `Cairo\Pool` now waits on its PHP thread, which keeps the timers itself,
  instead of on a goroutine: nothing crosses to Go, and a sleep or a channel
  hand-over between two fibers costs about a microsecond where it cost 35 to
  70. A thread whose fibers keep each other busy still takes new requests
  and Go's answers every 64 fibers. A thread that gave Go an operation and
  has nothing else to run watches for a few microseconds whether the answer
  is there before it sleeps, which that of a short operation is: about a
  third off the time of a Go operation when nothing else runs on the thread.
- Requests and answers cross between Go and the PHP threads in queues that
  neither side calls the other for. A thread used to call into Go for its
  next event and to hand over an answer, and each call held one of Go's
  processors, which Go's scheduler then took back and gave out again: a
  route that computes for 350 µs cost 5.7 thread switches per request. A
  thread now sleeps and wakes in C, and one goroutine collects the answers
  of all threads. Measured at 64 connections: 8% more requests per second
  on a route that answers at once and on one MySQL query, 10% more on the
  route that computes. `offload()` and a Go operation of a thread with
  nothing else to do take longer for now (about 28 µs and 6 µs), and one
  PostgreSQL query per request does 3% fewer requests.
- Performance: a fiber no longer has a goroutine and a context in Go.
  `async()` and the derived contexts (`withTimeout()`, `withDeadline()`,
  `withCancel()`) are PHP alone, a Go operation runs under its request's
  context, narrowed to the deadline of the context that asked, on a
  goroutine that then waits for the thread's next operation, and `cancel()`
  ends the operations in flight under the context. `async()` + `await()`
  takes 1.4 µs instead of 3.6, `withTimeout()` + `cancel()` 0.36 µs instead
  of 2.9, and the hello benchmark serves 2.0% more requests per second.
  Derived contexts now also work outside a request thread, and the internal
  `Context::$fiberId` is gone.
- Performance: a fiber that waits on a socket (PDO MySQL and everything else
  on PHP's streams, curl, the Revolt driver) no longer asks Go. Each PHP
  thread has an epoll of its own: the fiber arms its socket there and parks
  on the thread, the thread's loop asks once a turn which sockets are ready,
  and a thread with nothing to run sleeps in its epoll, where Go wakes it
  through an eventfd. The stream's timeout and the context's deadline are
  timers of the thread. One MySQL query for each request: 20% more requests
  per second with 22% less CPU for each, and 0.7 thread switches a request
  instead of 1.7; a curl transfer costs 18% less CPU. The wait of a
  millisecond on the thread itself, which stood in for this, is kept only
  where a thread serves one request at a time (isolated mode): next to other
  requests it cost 4 to 7%.

## [0.4.0] - 2026-11-19

### Added

- `App::slowRequests($seconds)`: log every request slower than that, with
  where its time went (SQL, HTTP, sockets, sleeps, waits, PHP), measured in
  Go with no code in the app (#16).
- `App::repeatedQueries()`: logs a warning when one request runs the same
  statement (by shape, without its literals) N times or more, through PDO
  with any driver or `Cairo\Sql`. Off by default (#17).
- `App::heavyRoutes($seconds)`: warn, once a minute per route, about a
  route that runs PHP for longer than that without yielding, so it can move
  the work to `offload()`. Measured in Go with no code in the app (#19).
- `Cairo\Sql\Db::open()` option `acquire_timeout`: a `Cairo\PoolTimeout`
  when no connection becomes free in time (#12).

### Changed

- A new request no longer goes to a PHP thread that has been running one
  fiber for 5 ms or more without pausing (a CPU loop, an unhooked blocking
  call), while another thread is free of that (#14).

### Fixed

- `Cairo\Sql`: a transaction opened with plain SQL on a pool no longer
  reaches the next request on that connection. `BEGIN`, `START TRANSACTION`,
  `SET autocommit`, `LOCK` and `XA` on `Db` throw a `SqlException` (use
  `transaction()`), a PostgreSQL connection that still comes back in a
  transaction is closed, and `mysql://` URLs refuse `multiStatements` (#12).

## [0.3.0] - 2026-11-19

Connection pools done well: bounded waits, a reset before reuse, connection
lifetimes, and one PostgreSQL pool for all threads.

### Added

- `Cairo\Pool` options: `acquireTimeout` (a `Cairo\PoolTimeout` when no
  connection becomes free in time, and a waiting request leaves the queue
  the moment it is cancelled), `maxLifetime` and `maxIdleTime` (retire old
  connections), and `reset` (run on every connection before it is reused).
- `cairo_pool=N` in a `pgsql:` DSN: PDO objects on every PHP thread share N
  server connections. Each borrows one for the request that uses it; a
  transaction left open is rolled back before the connection is reused.

### Changed

- `Cairo\Pool` rolls back a PDO transaction a request left open before the
  next request gets the connection, and drops a connection whose reset fails.
- A request past its deadline no longer receives a pooled connection it was
  waiting for.

### Performance

- `PDO::inTransaction()` on PostgreSQL no longer calls into Go: every reply
  carries the transaction status.

## [0.2.0] - 2026-11-18

A response cache in Go, and the stubs as their own Composer package.

### Added

- `->cache(ttl, staleWhileRevalidate, vary)` on an HTTP route: Go answers
  repeated GET requests from memory, merges identical requests that arrive
  together into one PHP call, and serves a stale response while one request
  rebuilds it. Responses with `Set-Cookie` or private `Cache-Control`, and
  requests with `Authorization` or `Cookie`, are not cached by default.
- `->cache(tags: [...])` and `Cairo\Cache::purge('tag', ...)`: drop the cached
  responses of tagged routes when the data behind them changes.
- A HEAD request to a cached route is answered from what a GET stored.

### Changed

- The stubs' Composer package is now `cairophp/stubs`, a mirror of `stubs/`
  published with each release; it replaces `cairophp/cairo`.

## [0.1.1] - 2026-11-18

Faster requests, socket waits and curl, and the static binary on PHP 8.5.

### Build

- The static binary is built on PHP 8.5.12 with opcache built in, and CI
  and the release workflow require opcache. The native build (`make php`,
  the Dockerfile) stays on PHP 8.3, so CI covers both.

### Changed

- `register_argc_argv` defaults to 0: PHP 8.5 deprecates deriving `argv`
  from the query string.

### Performance

- A hooked socket wait first polls the socket on the PHP thread for up to
  1 ms when the thread has nothing else to run, instead of going through Go: a
  database on the same network usually answers within that. PDO MySQL point
  queries at one connection went from 2.6k to 3.2k per second; busy threads
  are unchanged.
- A new curl handle takes over the kept-alive connections of a handle that was
  closed on the same thread (up to 4 per thread), so code that makes a handle
  per call, such as Laravel's `Http` client, stops opening a connection each
  time. A Laravel endpoint with two upstream calls went from 388 to 434
  requests per second.
- Request fibers are reused: a new fiber costs a stack mmap and, when freed, a
  TLB shootdown on every CPU. Hello-world requests went from 23k to about 40k
  per second on two cores.
- Go gets one scheduler slot (`GOMAXPROCS`) per PHP thread on top of one per
  CPU, so PHP threads calling into Go no longer queue behind net/http.
  `GOMAXPROCS` in the environment still wins.
- Go's garbage collector runs at `GOGC=400` unless `GOGC` is set: its live
  heap is small, so the default collected every few hundred requests (about
  10% more requests per second for about 12 MiB more memory).
- Events from Go to PHP (requests, I/O results) use a small binary format
  that C decodes straight into PHP arrays, instead of JSON text and
  `json_decode()`; request and response bodies cross as raw bytes instead of
  base64. Hello-world requests about 10% faster, a 64 KiB echo about 4x.
- Hooked `curl_exec()` keeps its connections alive between calls on one
  handle (each call used to open a new one): about 2x the requests per second.
- gRPC `ServerStream::send()` queues the reply without a fiber switch.
- Hooked socket waits (PDO MySQL, streams, curl) reach Go without JSON, and
  the poller keeps a socket in epoll between waits: about 9% more MySQL
  queries per second.
- `Cairo\Sql` on MySQL interpolates parameters by default
  (`interpolateParams=true`): one round trip per query instead of three,
  about 35% more queries per second.

### Fixed

- Hooked curl took 200 ms or more per transfer once the process had 1024 or
  more descriptors open (any busy server): it found curl's sockets with
  `curl_multi_fdset()`, which cannot hold higher numbers, and fell back to
  polling every 100 ms. `curl_exec()` now learns its sockets
  from curl's socket callback, and `curl_multi_select()` uses
  `curl_multi_waitfds()` where libcurl has it (8.8 and later, as in the static
  binary).
- Curl handles of failed `async()` tasks and cancelled requests are freed when
  the request ends: their exceptions kept them in reference cycles until PHP's
  collector ran, and each one held two descriptors. Cairo now collects cycles
  after a failure, at most once a second per thread.
- `Cairo\Pool` drops a connection whose request was cancelled while holding
  it, and `with()` drops it when the callback throws, instead of handing a
  broken connection to the next request.
- `Cairo\Sql` keeps `max_open` idle connections by default (was 2, which
  reconnected for most queries under load).
- A request cancelled by its client no longer logs a stack trace.

## [0.1.0] - 2026-11-17

The first release: one static binary that serves HTTP and gRPC declared in
PHP, with many requests per PHP thread as fibers.

### Serving

- `cairo serve server.php`: servers, routes and gRPC services declared in PHP,
  served by Go's net/http and grpc-go, on one pool of PHP threads (#1).
- Every request in its own fiber with its own goroutine; `async()`, `await()`,
  `all()`, `Cairo\sleep()` (#1), `Cairo\Channel` and `select()` (#25).
- `Cairo\Context`: deadlines, cancellation and per-fiber values that child
  fibers inherit (#2), with `withTimeout()`, `withDeadline()`, `withCancel()`,
  `cancel()`, `run()`, `value()` (#26) and `done()` (#25).
- Hard deadline stop, even in CPU loops: `DeadlineExceeded`, then a forced
  unwind (#19).
- Go middleware declared from PHP: request ids, access log, CORS, rate limits,
  body limits, timeouts and gzip (#5); panic recovery (#30).
- Isolated mode for code that is not fiber-safe, with superglobals, `echo`,
  `header()` and `Cairo\Resettable` resets (#6); file uploads (#7).
- PSR-7 and PSR-15 handlers, Symfony and Laravel kernels, `$http->fallback()`
  (#27, #38).
- `offload()` to a separate pool of CPU threads (#1), with task objects (#32).
- Thread supervision: crashed PHP threads are replaced, and threads are
  recycled by request count or memory (#23).
- `App::maxInFlight()` backpressure (#31) and `App::detectBlocking()` (#33).
- Cairo's own SAPI, `PHP_SAPI === 'cairo'` (#20).

### gRPC

- `cairo gen`: typed PHP messages, enums, service interfaces and clients from
  `.proto` files, with no protoc (#4, #16).
- Unary and all streaming kinds, server and client (#17); health and reflection
  built in (#1); `Cairo\Grpc\Client` (#16).

### Runtime hooks

- Blocking PHP functions suspend only their fiber: `sleep()` and friends,
  sockets and streams (#2), `connect`, DNS and curl (#3), `curl_multi_select()`
  (#8).
- TLS through Go for `ssl://`, `tls://` and `https://` (#9), client
  certificates (#11), STARTTLS (#14) and turning TLS off (#22).
- `Cairo\Pool` for per-fiber connections (#3).

### Go-backed APIs

- `pdo_pgsql` backed by pgx, no libpq (#10), with bytea streams (#11),
  LISTEN/NOTIFY and COPY (#12) and large objects (#21).
- `Cairo\Http\Client` on Go's net/http (#13).
- `Cairo\Net`: connections, listeners, datagrams and DNS (#15).
- `Cairo\Sql`: PostgreSQL (#36) and MySQL/MariaDB (#40) through database/sql,
  with a process-wide pool and fiber-bound transactions.
- `Cairo\Revolt\Driver`: Amp v3 and other Revolt libraries on Cairo's
  scheduler (#35).

### Build and tooling

- PHP built with opcache and the common extensions (#18).
- One static musl binary, `make static`, with cached library and PHP layers
  (#24, #37), for amd64 and arm64.
- IDE stubs and a Composer package with a PHPStan include (#34); typed gRPC
  streams and generated messages for static analysis (#39).
- License notices for adapted code (#29); Cairo is licensed under Apache-2.0.

### Fixed

- `Cairo\Sql` returns timestamps in UTC instead of the server's time zone
  (#40).

[Unreleased]: https://github.com/cairophp/cairo/compare/v0.4.0...HEAD
[0.4.0]: https://github.com/cairophp/cairo/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/cairophp/cairo/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/cairophp/cairo/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/cairophp/cairo/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/cairophp/cairo/releases/tag/v0.1.0
