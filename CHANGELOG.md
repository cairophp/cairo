# Changelog

All notable changes to Cairo. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/). Before 1.0, a minor version may
change the PHP API; patch versions do not.

## [Unreleased]

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

[Unreleased]: https://github.com/cairophp/cairo/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/cairophp/cairo/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/cairophp/cairo/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/cairophp/cairo/releases/tag/v0.1.0
