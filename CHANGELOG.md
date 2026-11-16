# Changelog

All notable changes to Cairo. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/). Before 1.0, a minor version may
change the PHP API; patch versions do not.

## [Unreleased]

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
  (#24, #37), for amd64 and arm64, on PHP 8.5 with opcache built in.
- IDE stubs and a Composer package with a PHPStan include (#34); typed gRPC
  streams and generated messages for static analysis (#39).
- License notices for adapted code (#29); Cairo is licensed under Apache-2.0.

### Fixed

- `Cairo\Sql` returns timestamps in UTC instead of the server's time zone
  (#40).

[Unreleased]: https://github.com/mohamed-samir907/cairo/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/mohamed-samir907/cairo/releases/tag/v0.1.0
