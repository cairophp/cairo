# Changelog

All notable changes to Cairo. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/). Before 1.0, a minor version may
change the PHP API; patch versions do not.

## [Unreleased]

### Added

- HTTPS and HTTP/2 on the HTTP server, so that Cairo needs no proxy in front
  of it. `$http->tls($certFile, $keyFile)` serves a certificate from two PEM
  files and reads them again when they change. `$http->autoTls($domains,
  $cacheDir)` gets and renews certificates from Let's Encrypt, or from
  another ACME authority. Clients that ask for HTTP/2 get it, TLS 1.2 is the
  oldest version accepted, and `$http->redirectHttp()` adds a plain listener
  that sends its requests to the HTTPS one.
- `$http->h2c()`: HTTP/2 without TLS, for a load balancer that speaks it to
  its backends.
- `$grpc->tls($certFile, $keyFile)`: gRPC over TLS.
- `Request::protocol()` and `Request::secure()`.
- An official image, `ghcr.io/cairophp/cairo`, for amd64 and arm64: the
  release's static binary on Alpine, running as the user `cairo`. The release
  workflow publishes it with every release, tagged `x.y.z`, `x.y` and
  `latest`.
- `install.sh`, which installs the release binary for the machine it runs
  on after checking it against `SHA256SUMS`.
- `SECURITY.md`: how to report a vulnerability, and which versions get fixes.

## [0.5.0] - 2026-11-23

Mostly speed, and a smaller API. A fiber no longer has a goroutine: sleeps,
channels and socket waits park on their own PHP thread, and Go is asked only
for what Go does. On the benchmark machine, plain PHP serves about 35% more
requests per second, a MySQL route about 70% more, gRPC streams 3 to 4 times
as many messages, and uploads and large answers about twice as many.

**Upgrading from 0.4.** This release removes `Cairo\Sql`, `Cairo\Http\Client`,
the Revolt driver, `App::repeatedQueries()` and the `cors()`, `rateLimit()`
and `gzip()` middleware; Removed below says what to use instead. Two
smaller changes can reach existing code: a fiber whose offload thread dies
gets an `OffloadException` (was `Context\Canceled`), and
`Grpc\ClientStream::send()` returns once the request is queued, so a write
error comes from the next `send()` or `closeSend()`. Update the stubs
package to `^0.5`, and use `cairophp/octane` with this release for
Laravel's fiber mode.

### Added

- `Cairo\RequestLocal::register($class, ...$properties)`: static
  properties that keep one value per request, swapped in C when fibers of
  different requests take turns on a thread. The first step to running
  Laravel without isolated mode (#22).
- `$http->publicDir($dir)`: serve a document root (Laravel's `public/`) from
  Go, with every path that is not a file going to the routes. PHP files and
  hidden files are never served (#24).
- `App::detectLeaks()`: a development aid that logs the static properties a
  request changed but that are not request-local, so one request could read
  another's value. Caches that only gain keys are not reported (#25).

### Changed

- Isolated mode: requests wait in Go in one first-in, first-out queue and go
  to the next free thread, instead of queuing on a thread behind its slow
  requests. Laravel (Octane) p99 at 256 connections: 2.1 s to 0.6 s (#23).
- Internal, no change in behaviour: Cairo is split into a core engine and
  extensions. The core (`internal/core`, with its PHP files in
  `internal/core/php`) has the PHP threads, the fiber scheduler, dispatch,
  waiting and the op registry; the servers, clients, hooks, database drivers
  and diagnostics are extensions in `internal/sapi`, `php/` and the server
  packages, and join the core only through what it exports
  (`core.RegisterOp`, `RegisterPHP`, `RegisterC` and the rest; `AGENTS.md`
  has the list). A test fails when the core names an extension in Go, C or
  PHP (`internal/core/boundary_test.go`). Ten move-only steps, measured at no
  cost (#26 to #35).
- Performance: Cairo's own C code is compiled with `-O2`. The build scripts
  set `CGO_CFLAGS` to PHP's include paths, which dropped the optimization
  that Go asks for by default, so every build had compiled that code
  without any. A Go operation or a wait among 64 fibers is about 8% cheaper,
  and the PostgreSQL benchmark route serves 3.6% more requests per second (#36).
- Performance: opcache holds Cairo's own PHP files. The engine writes them
  when the server starts, and opcache leaves out a file that changed in the
  last two seconds, so every PHP thread compiled the runtime itself, with no
  optimizer and nothing shared between threads. The files now get an old
  modification time (#36).
- Performance: on glibc builds, a call from a PHP fiber into Go no longer
  makes Go's runtime fetch the thread's stack from the system each time (a
  `sched_getaffinity` system call and an allocation). Cairo reads each
  thread's stack once and answers from that (#36).
- Performance: the engine keeps the calls a PHP thread took and the fibers
  that live on it in tables of that thread, which only the thread itself
  uses, in place of two maps shared and locked by all threads (#36).
- Performance: a request's bookkeeping in Go (its call, its root fiber,
  their channels and the buffer of its event) is reused from one request to
  the next instead of allocated for each: eight allocations and about 1 kB
  less per request (#36).
- Performance: a request's fibers wait under the context of the server's
  request itself. The engine made a context of its own under it for every
  request and ended it when PHP answered, which the server's context does
  anyway once the request is done: five allocations less per request (#36).
- Performance: a request goes to a PHP thread that is already running PHP
  and will be back for its next event within 50 µs, when there is one,
  instead of waking a thread that sleeps. A thread that holds 32 more
  requests than the least busy one, or that has run one fiber for long, is
  still passed over (#36).
- Performance: the arguments of a Go operation (the gRPC
  and network clients, the cache purge) cross from PHP to Go in the binary
  format that results already come back in, instead of as JSON text: no
  `json_encode()` in PHP, and Go reads them about three times as fast. An
  operation that runs on the PHP thread gets its result back the same way. A
  string argument may now hold any bytes, where JSON refused text that was
  not UTF-8 (#36).
- Performance: a fiber in `Cairo\sleep()`, in a hooked `sleep()` or
  `usleep()`, or blocked on a `Cairo\Channel`, in `select()` or on a
  `Cairo\Pool` now waits on its PHP thread, which keeps the timers itself,
  instead of on a goroutine: nothing crosses to Go, and a sleep or a channel
  hand-over between two fibers costs about a microsecond where it cost 35 to
  70. A thread whose fibers keep each other busy still takes new requests
  and Go's answers every 64 fibers. A thread that gave Go an operation and
  has nothing else to run watches for a few microseconds whether the answer
  is there before it sleeps, which that of a short operation is: about a
  third off the time of a Go operation when nothing else runs on the thread (#36).
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
  PostgreSQL query per request does 3% fewer requests (#36).
- Performance: a fiber no longer has a goroutine and a context in Go.
  `async()` and the derived contexts (`withTimeout()`, `withDeadline()`,
  `withCancel()`) are PHP alone, a Go operation runs under its request's
  context, narrowed to the deadline of the context that asked, on a
  goroutine that then waits for the thread's next operation, and `cancel()`
  ends the operations in flight under the context. `async()` + `await()`
  takes 1.4 µs instead of 3.6, `withTimeout()` + `cancel()` 0.36 µs instead
  of 2.9, and the hello benchmark serves 2.0% more requests per second.
  Derived contexts now also work outside a request thread, and the internal
  `Context::$fiberId` is gone (#36).
- Performance: a fiber that waits on a socket (PDO MySQL and everything else
  on PHP's streams, curl) no longer asks Go. Each PHP
  thread has an epoll of its own: the fiber arms its socket there and parks
  on the thread, the thread's loop asks once a turn which sockets are ready,
  and a thread with nothing to run sleeps in its epoll, where Go wakes it
  through an eventfd. The stream's timeout and the context's deadline are
  timers of the thread. One MySQL query for each request: 20% more requests
  per second with 22% less CPU for each, and 0.7 thread switches a request
  instead of 1.7; a curl transfer costs 18% less CPU. The wait of a
  millisecond on the thread itself, which stood in for this, is kept only
  where a thread serves one request at a time (isolated mode): next to other
  requests it cost 4 to 7% (#36).
- Performance: an `offload()` job goes from its request thread straight into
  the inbox of an offload thread, and its result straight back, with no
  goroutine on the way and no base64 or JSON around the serialized bytes:
  two wake-ups of threads where it took four. An empty job takes 36 µs where
  it took 100 µs, and a job of 50 µs costs 87 µs of CPU where it cost 165
  µs. The wait still ends with the fiber's context while the job runs on. A
  fiber whose offload thread stops in the middle of its job now gets an
  `OffloadException` (it was `Context\Canceled`), and a result that cannot
  be serialized is an `OffloadException` for its caller instead of the end
  of the offload thread (#36).
- `Cairo\Channel`: `send()` into a channel with room and `recv()` of a
  buffered value no longer go through `select()`, and neither do `trySend()`
  and `tryRecv()`: a send and a receive with nobody waiting take 0.1 µs
  instead of 1.2 µs, and a `select()` that finds a channel ready less than
  half its time. Nothing changes in what they do (#36).
- The scheduler of a PHP thread keeps its fibers in C: the context each
  fiber runs under, the fibers that wait for their next task, the queue of
  those to resume, and the parked waits with their timers. A `sleep(0)`
  takes 0.7 µs instead of 1.4 µs, `Context::current()` 33 ns instead of 69
  ns, and a route that queries MySQL through PDO, which parks its fiber for
  every wait on the socket, answers 16% more requests per second. Fibers
  that sleep until the same moment wake in the order they went to sleep (#36).
- Performance: a gRPC message goes between protobuf's bytes and PHP's array
  directly, with no message object and no JSON text in between, in the
  server and in `Cairo\Grpc\Client`. Handlers and generated classes see the
  same arrays as before; a message with a `google.protobuf` type, a type
  that is not proto3 and any unusual value still go through protojson.
  Unary calls served: 11% more per second (ghz at 64 concurrent calls, 4
  PHP threads on 2 CPUs); a route that makes one through the client: 3 to
  8% more (#39).
- Performance: a gRPC stream no longer costs a Go operation per message, on
  either side. `send()` of a handler's stream and of a client's stream
  queues the message in Go with one call, and Go reads what the other side
  sends ahead of PHP (64 messages or 1 MiB at most), so that one `recv()`
  brings all that has arrived. With 100 messages per call: a handler that
  sends them answers 1,539 to 4,337 calls per second and one that reads
  them 977 to 4,082 (ghz, as above); a route that reads them through the
  client 802 to 3,140 requests per second and one that sends them 811 to
  3,244 (wrk at 32 connections) (#39).
- gRPC, at the edges: a reply that holds `NAN`, `INF` or a string that is
  not UTF-8 ends its call with INTERNAL (the handler's fiber ended with an
  uncaught exception before). `ClientStream::send()` returns when the
  request is queued, as `ServerStream::send()` does: a request that could
  not be written is thrown by the next `send()` or by `closeSend()`, which
  returns once all are written. A message that cannot be read ends the
  reading of its stream, where the next `recv()` went on to the message
  after it (#39).
- Performance: PDO PostgreSQL sends each request to Go, and gets its
  answer, as an array in Cairo's binary form instead of JSON text, and the
  parameters of a statement, COPY data and the bytes of a large object are
  no longer base64 on the way. A route with one query: 2.6% more requests
  per second (wrk at 64 connections, 4 PHP threads on 2 CPUs) (#39).
- Performance, HTTP: a request's way between net/http and PHP allocates
  less (21 allocations where there were 45, for a request with a browser's
  headers): PHP's answer is decoded from one copy with one slice for all
  header values, which the response then shares. A request with 13 headers,
  two MySQL queries and a 1.5 KB JSON answer: 4% more requests per second (#40).
- Performance, HTTP: a route with `->cache()` looks in the cache before it
  reads the request, so a request that the cache answers builds nothing for
  PHP: 6% more requests per second on a cached page. A GET that carries a
  body gets the cached answer without the body being read, also a body past
  the 32 MiB limit, which was answered with 413 before (#40).
- Performance, HTTP: large bodies are copied less. An upload is read into
  one buffer sized from its `Content-Length` (of which 1 MiB is taken on
  trust; past that the buffer grows with what arrives) and copied once on
  its way to PHP: 1 MB uploads about twice as many per second, 100 KB
  uploads too. An answer of 32 KiB or more is written to the client from
  the memory PHP's thread handed over, without a copy in Go: 58% more 1 MB
  answers per second, 50% more of 100 KB. A request whose `Content-Length`
  is past the 32 MiB limit is refused with 413 before its body is read (#40).
- Performance: `Request::header()` finds a name in its usual spelling
  (`Content-Type`) with one lookup instead of a loop over all headers, and
  `Request::query()` parses the query string once per request (#40).
- Performance: a read or a write on a hooked socket (PDO MySQL, streams) is
  tried at once, and the fiber waits only when the socket is not ready.
  Before, each one first asked the kernel whether the socket was ready. A
  request with two MySQL queries makes 4 system calls fewer, of 20; its
  requests per second do not change measurably (#41).
- A thread that is recycled (`maxRequests()`, `maxMemory()`) serves until
  its replacement has run `server.php`. Before, the new thread took its
  place while it still booted, and requests waited behind the boot: with
  Laravel through Octane, the slowest 0.1% of requests under a light load
  took 15 ms and now take 8 ms. For the length of the boot the old thread
  serves past its limit and both hold their memory (#41).
- Performance: a fiber that waits on a hooked socket (a PDO MySQL query, a
  stream, curl) is armed in its thread's epoll and parked by one call into
  C, and the thread's loop wakes it without running PHP. Before, each wait
  ran seventeen PHP functions and made an object, and the loop called PHP for
  every socket that was ready. A wait takes 2 µs less of the thread's time
  (5.2 to 3.2 µs, measured inside the server). The requests per second of a
  request with two MySQL queries rise by about 2%, which is inside what the
  benchmark machine can tell apart (#42).

### Removed

- `App::repeatedQueries()`, the repeated query detector. Find N+1 queries
  with the tools of your framework (Laravel's `preventLazyLoading()`, the
  Symfony profiler) or the database's own statement log (#38).
- `Cairo\Sql` (`Db`, `Tx`, `SqlException`, `Bytes`). PDO is the way to a
  database: `new PDO('pgsql:...')` is Cairo's Go-backed driver and takes
  `cairo_pool=N` for a pool shared by all threads, and PDO MySQL waits
  without blocking through the socket hook. The binary no longer carries
  the Go MySQL driver (#38).
- `Cairo\Http\Client`. Use curl, or a client built on it such as Guzzle:
  `curl_exec()` suspends only its fiber, and the slow request log still
  counts the call under `http` (#38).
- `Cairo\Revolt\Driver`, the driver for Revolt, Amp and ReactPHP's fiber
  adapters. Apps written for those event loops no longer run on Cairo's
  scheduler (#38).
- `Middleware::cors()`, `Middleware::rateLimit()` and `Middleware::gzip()`.
  Set these in the reverse proxy in front of Cairo or in the framework.
  `requestId()`, `accessLog()`, `maxBody()` and `timeout()` stay (#38).

### Fixed

- Stopping the server, or a thread that is recycled, no longer cuts a request
  off in the middle: a thread finishes what it holds before it ends, also
  the requests whose client has left and the fibers that still wait for Go.
  Before, PHP destroyed those fibers where they waited, and fiber mode
  could log `FiberError: Cannot suspend in a force-closed fiber` for each
  (seen under Laravel when a stop met a MySQL connect in flight). The same
  line after a handler's `exit()` is gone too (#37).
- PDO PostgreSQL with `cairo_pool=N`: a statement on a PDO object that was
  just taken from a `Cairo\Pool` could fail with "the connection is busy
  with another query" while the request that had the object before gave its
  server connection back (7 in 490,000 requests in a benchmark) (#37).
- gRPC: a damaged request no longer stops the server. protobuf-go panics on
  a map entry whose key comes a second time with another wire type, and in
  a client-streaming or bidirectional method nothing recovered that panic.
  Such a message now ends its call with INTERNAL, like any other that
  cannot be read (#39).
- `Cairo\Grpc\Client`: the same damaged message in a reply, from a server
  that an app calls, no longer stops the Cairo process that made the call.
  The call ends with INTERNAL (#39).
- `App::detectBlocking()` no longer blames one fiber for the time of
  several. Fibers that became ready at the same moment (their sleeps ended
  together, or one request began several tasks) run one after the other,
  and the detector counted them as one: ten that each computed for 30 ms
  were reported as a request that kept its thread for 120 ms. It now counts
  each fiber from where it began. Code that really blocks is reported as
  before (#43).

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

[Unreleased]: https://github.com/cairophp/cairo/compare/v0.5.0...HEAD
[0.5.0]: https://github.com/cairophp/cairo/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/cairophp/cairo/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/cairophp/cairo/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/cairophp/cairo/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/cairophp/cairo/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/cairophp/cairo/releases/tag/v0.1.0
