# Changelog

All notable changes to Cairo. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/). Before 1.0, a minor version may
change the PHP API; patch versions do not.

## [Unreleased]

### Added

- Native images, `ghcr.io/cairophp/cairo:0.7-php8.6-trixie` and so on for
  each PHP (`trixie` alone for the newest): Cairo built against the official
  `php` image's own thread-safe PHP, so an app adds the extensions it needs
  with that image's `docker-php-ext-install`, `pecl install` or
  `install-php-extensions`, and Cairo loads them.
- A static binary with more extensions built in: `Dockerfile.static` takes
  PHP's own extensions that are off by default and PECL packages
  (`EXTRA_EXTENSIONS`, `PECL_EXTENSIONS`, and `APK_PACKAGES` for the static
  libraries one needs). A static binary cannot load extensions at run time.
- A static binary for glibc that loads extensions, `cairo-php8.6-linux-amd64-gnu`
  and so on for each PHP and architecture (`cairo-linux-amd64-gnu` for the
  newest), built with `Dockerfile.gnu`: everything but glibc is linked in,
  and it runs where glibc is 2.36 or newer. Its kit,
  `cairo-php8.6-linux-amd64-gnu-dev.tar.gz`, has that PHP's headers,
  `php-config` and `phpize`, so `phpize && ./configure && make install`
  builds an extension for it. `CAIRO_LIBC=gnu` makes `install.sh` install
  both.
- A release binary and an image for each PHP that Cairo supports: 8.3, 8.4,
  8.5 and 8.6. The binaries are `cairo-php8.4-linux-amd64` and so on, and
  the images `ghcr.io/cairophp/cairo:0.6-php8.4` (also `x.y.z-php8.4` and
  `php8.4`, the newest release with that PHP). The default binary
  (`cairo-linux-amd64`) and the plain image tags (`x.y.z`, `x.y`, `latest`)
  have the newest PHP. `CAIRO_PHP=8.4` makes `install.sh` install that one.
  With PHP 8.3 a server cannot see opcache's restart coming and the threads
  of a reload do not use opcache, as with any Cairo built on 8.3.
- The static binaries with PHP 8.3 and 8.4 have opcache built in, with
  static-php-cli's patches, without its JIT. Before, a static build with a
  PHP older than 8.5 had no opcache.

### Changed

- The default binary and image have PHP 8.6 (they had 8.5).
- `make php` and the Dockerfile build PHP 8.6.0 (they built 8.3.27).

### Fixed

- A stop signal that came just as `cairo serve` began to answer could end the
  process at once, without `--drain`, the grace time or the last spans: the
  signals are now taken before the first server starts.
- `make build` after switching to another PHP (`PHP_PREFIX`) could link code
  compiled against the old PHP's headers from Go's build cache, and fail to
  link or crash at start. The PHP version is now part of the cache key.
- With PHP 8.6.0 and no `error_reporting` in php.ini, every error inside a
  fiber was silenced (PHP 8.6.0 starts a fiber with php.ini's value, which is
  then 0), so a request handler's warnings and fatal errors were neither
  shown nor logged. Cairo now makes `E_ALL` the default, as it is in PHP.

## [0.6.0] - 2026-11-26

Cairo now does what PHP-FPM, FrankenPHP and RoadRunner do, in one binary.
It serves a folder of scripts (WordPress) as PHP-FPM does, with no
declaration and as fast: `cairo serve public/`. It streams answers and
serves WebSockets, topics and a Mercure hub, speaks HTTPS and HTTP/2 with
certificates from Let's Encrypt, and reloads the code of a running server
with `--watch` or a signal. It has metrics, health probes and OpenTelemetry
traces, and PHP's command line in the binary (`cairo php`, with the fiber
scheduler and signals). There is also an official image and a runtime for
Symfony (`cairophp/symfony-runtime`).

**Upgrading from 0.5.** No API was removed. A few changes can reach
existing code:

- **A declared classic app whose every route is `scripts()`.** server.php
  runs only when a thread starts. What it sets in PHP (`ini_set()`,
  constants, functions) no longer reaches the scripts: put it in php.ini or
  an `auto_prepend_file`.
- **Classic scripts.** Cookie names follow PHP's rules (`a.b` is
  `$_COOKIE['a_b']`), and PHP's own waits hold the thread, as under PHP-FPM.
- **`putenv()`.** It changes the request's environment, no longer the
  process's.
- **The released binaries and the image.** They contain the Mercure hub,
  which is AGPL-3.0. Build with `TAGS=nomercure` for a binary without it.
- **Building from source.** It needs Go 1.27.

Update the stubs package to `^0.6`, and the image tag to `0.6`.

### Added

- `cairo serve` on a folder, or on a PHP file that declares no `Cairo\App`,
  serves it in classic mode as nginx and PHP-FPM do, with no server.php:
  a folder is the document root, a plain file is the front controller.
  Cairo tells a declaration with PHP's tokenizer and never runs the file to
  find out. `--listen`, `--threads` and `--timeout` set what a declaration
  would (without `--timeout`, php.ini's `max_execution_time` is the
  deadline), and each request runs only its script.
- Classic mode, for apps that are a set of PHP scripts (WordPress, Drupal,
  a folder of `.php` files): `App::concurrency('classic')` gives every
  request a PHP request of its own, as PHP-FPM does, on the same PHP thread,
  and `$http->scripts($dir)` maps each request to a script under `$dir` as
  nginx's `try_files` does (the first `.php` part of the path with the rest
  as `PATH_INFO`, a directory's `index.php`, else the root `index.php`),
  with Go serving the other files. Scripts run in the global scope with
  `auto_prepend_file`, `exit()` and shutdown functions as under PHP-FPM, and
  get `php://input`. WordPress 6.8 installs and runs on it. See
  `examples/classic`.
- `Cairo\App::serve()`: a file that `cairo serve` runs can serve its App
  itself instead of returning it, as a framework's runner does, and may end
  with `exit(0)` afterwards. The package `cairophp/symfony-runtime` uses it
  to serve a Symfony app through its own front controller, with no
  `server.php`: `cairo serve public/index.php`. See `examples/serve`.
- PSR-15 apps built on laminas-diactoros (Mezzio), slim/psr7 or
  php-http/discovery's factories get their PSR-7 requests from those, as
  they did from nyholm/psr7 and guzzlehttp/psr7.
- Reloading the code of a running server. `cairo serve --watch` looks at the
  files of the app (`*.php`, `*.twig`, `*.yaml`, `*.yml`, `.env*` and
  `composer.lock` under the server file's directory, or under each
  `--watch-path`; not `vendor`, `node_modules`, `storage`, `var`) and gives
  the server the new code when one changes, and `kill -USR1 <pid>` does the
  same once, for a deploy. No request is refused or fails: each PHP thread is
  replaced by one that has read the code anew, as a recycled thread is, and
  finishes the requests it holds with the code they began with. The first
  new thread is tried alone, so code that does not start (a parse error, an
  exception in `server.php`) replaces nothing and the server runs on. What
  `server.php` declares (servers, routes, settings) is built once per
  process: when that changes, or a `.env` file does, `--watch` starts the process anew in place, and the signal
  refuses another declaration and says so. A reload that fills opcache ends
  in such a restart too (with a PHP older than 8.4, which cannot tell of it,
  the threads of a reload compile for themselves and leave opcache alone).
  The watcher looks at the files' times every 250 ms instead of using the
  kernel's file events, so it needs no dependency and works in a directory
  shared with a container. See `examples/reload`.
- The Mercure hub. `$http->mercure()` makes a server the hub of the
  [Mercure](https://mercure.rocks) protocol at `/.well-known/mercure`: Go
  serves every subscriber, so a subscriber holds no PHP thread, fiber or
  memory, and `mercure_publish()` publishes from PHP with one call, with the
  parameters it has in FrankenPHP. Tokens (`issuer()`), anonymous
  subscribers, private updates, history in a file (`history()`), the
  subscription API and clients from before Mercure 1.0 (`compatibility(8)`)
  are there. **The hub is the Mercure.rocks hub, which is AGPL-3.0, not
  Apache-2.0**: the released binaries and the official image contain it, and
  the build tag `nomercure` gives a binary without it (`make build
  TAGS=nomercure`). See `third_party/NOTICES` and `examples/mercure`.
- Streamed answers. `Response::stream(fn (Stream $out) => ...)` sends the
  head and then every `$out->write()` while the handler runs, and
  `Response::events(fn (Events $events) => ...)` is Server-Sent Events on
  top of it. Each stream is a fiber: a PHP thread holds as many as it has
  clients, a slow client holds back only its own handler, and the handler
  learns that its client left (`Context\Canceled`). `->noTimeout()` takes a
  route out of `Middleware::timeout()`, and the server's write timeout
  bounds each write of a stream instead of the whole answer. A server that
  stops ends its streams at their next write, in the same way, instead of
  waiting for them until its grace time is over.
- In isolated mode `echo`, `header()` and `flush()` are the answer, as under
  any PHP server: `flush()` sends what was written so far,
  `headers_send(103)` sends Early Hints and `fastcgi_finish_request()` ends
  the answer while the handler works on.
- `Request::earlyHints()`: 103 Early Hints from a handler.
- `$http->sendfile($dir)`: an answer names a file and Go sends it, with
  ranges and `If-Modified-Since`, while the PHP thread is free.
  `Response::file($path)` names one, and so do the headers `X-Sendfile`
  and, under a prefix, `X-Accel-Redirect`, as code written for Apache or
  nginx sets them. A path cannot leave the directory.
- `->streamBody()` on a route and `Request::read()`: the handler reads the
  request's body in pieces while the client sends it, so an upload is never
  in memory and a handler can answer while its client still sends (full
  duplex, on HTTP/1 too).
- Symfony's and Laravel's streamed responses (`StreamedResponse`,
  `response()->stream()`, `eventStream()`, downloads) are sent while they
  are made instead of being collected first, and a PSR-7 body of unknown
  size or above 1 MiB is sent in pieces. See `examples/streaming`.
- WebSockets. `Response::websocket(fn (WebSocket $ws) => ...)` makes the
  request's connection a WebSocket and runs the function with it in the
  request's fiber: `$ws->receive()`, `$ws->send()`, `$ws->close()`. The
  route's handler runs first, so it can answer 401 instead, and a page of
  another site is refused unless its origin is named. One PHP thread holds
  as many sockets as it has clients; a slow client holds back only its own
  handler; Go pings the clients and drops one that does not answer. A
  server that stops tells its clients to go away (status 1001).
- Topics. `Topic::publish($topic, $data)` is one call, and Go sends the
  message to every client that listens: the WebSockets that called
  `$ws->subscribe()`, and the Server-Sent Events clients of
  `Response::eventsFrom($topics)`, which hold no PHP memory and no fiber
  while they listen. A client that does not keep up is dropped instead of
  holding the others back. The topics are the process's own and keep
  nothing. See `examples/websocket`.
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
- Observability. `$http->metrics()` serves the process's metrics in
  Prometheus' format: every HTTP request and gRPC call by route and status
  with its duration (`cairo_http_request_duration_seconds`,
  `cairo_grpc_call_duration_seconds`), the PHP threads (`cairo_php_*`), the
  Go runtime and the process. `$http->health()` answers liveness and
  readiness probes without PHP, and `cairo serve --drain 5s` keeps serving
  for that long after a stop signal while the readiness probe answers 503.
  `$app->tracing()` traces every request and gRPC call with OpenTelemetry,
  continues the trace a request arrives in, sends the spans with OTLP over
  HTTP, and gives PHP the span to pass on (`$ctx->value('traceparent')`, and
  the `traceparent` header); calls through `Cairo\Grpc\Client` carry it by
  themselves. Nothing is measured or traced unless `server.php` asks for
  it. See `examples/observability`.
- `install.sh`, which installs the release binary for the machine it runs
  on after checking it against `SHA256SUMS`.
- `SECURITY.md`: how to report a vulnerability, and which versions get fixes.
- `cairo php`: PHP's command line in the binary, for `artisan`, `bin/console`,
  Composer and any script, with no PHP installed next to Cairo. It runs a
  file, code (`-r`) or standard input with the SAPI `cli`, `$argv`, `STDIN`,
  `STDOUT` and `STDERR`, PHP's exit status and the options `-d`, `-c`, `-n`,
  `-l`, `-i`, `-m`, `-v`, `--ini` and `--ri`. The same extensions as the server
  are there, Cairo's PDO pgsql driver included. `PHP_BINARY` is the binary,
  which runs a script when it is given PHP's arguments, so that PHP code can
  start PHP again; under the name `php` (a link, which the official image
  has) it is the command line as well. Not there: the interactive shell
  (`-a`) and the built-in web server (`-S`).
- A script of `cairo php` runs under Cairo's fiber scheduler, as a request
  handler does. `async()`, `await()`, channels, `select()`, contexts with a
  timeout and Cairo's clients work in it, and a wait in PHP's own functions
  (`sleep()`, PDO, curl, streams) suspends only its fiber: 50 fibers that
  each sleep a second, or 20 that each run a query of a second, take a
  second together. A script that runs one fiber at a time waits as it does
  under PHP and costs what it did. `CAIRO_SCHEDULER=off` runs a script
  outside the scheduler, for a program that has an event loop of its own for
  fibers (amphp, Revolt).
- Signals in the scripts of `cairo php`, with PHP's own `pcntl`: a script's
  handlers (`pcntl_signal()`, `pcntl_async_signals()`, `pcntl_alarm()`) run
  when a signal arrives, so a queue worker stops after the job in hand and
  gives its jobs a time limit, and Laravel's `octane:start` and Symfony's
  console signals work. Go receives the signal and brings it to the script:
  a script with one fiber is interrupted as under PHP (`sleep()` returns
  early), and with several the handler runs inside a fiber, at once, while
  the others wait. There a handler may not wait itself, as PHP lets no
  fiber switch inside one: it hands the signal to a fiber
  (`Channel::trySend()`). `pcntl_fork()` and the functions that wait for a
  signal are left out, and a server has none of pcntl.
- `CAIRO_PHP_CLI=/path/to/php` hands `cairo php` commands to that PHP
  instead of running them in the binary, for a command that needs what an
  ordinary PHP has and Cairo's does not (`pcntl_fork()`, an extension of
  your own).

### Changed

- A folder or plain script served with no declaration runs each request as
  PHP's own request, as PHP-FPM does, with none of Cairo's PHP around the
  script: PHP reads the cookies and parses the forms and uploads itself, and
  `fastcgi_finish_request()` and `headers_send()` are C functions. A real
  GET went from 3,987 to 4,965 req/s, level with PHP-FPM (4,980), and hello
  +43%. Cookie names follow PHP's rules there (`a.b` is `$_COOKIE['a_b']`).
- A declared classic app whose every route is `scripts()` is served as a
  folder with no declaration is: each request is PHP's own request, with no
  Cairo PHP around the script, and server.php runs only when a thread
  starts, never before a request. What server.php sets in PHP (`ini_set()`,
  constants, functions, files it requires) no longer reaches the scripts:
  put it in php.ini or in an `auto_prepend_file`, as under PHP-FPM. Its
  middleware and other Go settings apply as before. A real GET went from
  3,236 to 4,997 req/s, level with the same folder served with no
  declaration (5,013), and hello from 8,572 to 21,402. Cookie names follow
  PHP's rules there too. An app with other routes beside `scripts()` is
  served as before.
- A classic script under `scripts()` gets `PHP_AUTH_USER` and `PHP_AUTH_PW`
  (or `PHP_AUTH_DIGEST`) from the `Authorization` header, as PHP's own
  request does.
- `putenv()` changes the PHP request's own environment, which `getenv()`
  reads first, and never the process's: in classic mode each request's, in
  isolated and fiber mode the thread's. PHP's `putenv()` wrote the
  process's environment while other threads read it, which crashed the
  process within seconds when many classic requests called it (Laravel's
  `.env` loader does, 47 times a request), and let one request see what
  another set. C libraries that read the environment themselves do not see
  a `putenv()`.
- A classic script's environment functions work as under PHP-FPM: its CGI
  variables (`REQUEST_METHOD`, `HTTP_HOST` and the other headers,
  `SCRIPT_FILENAME`, ...) are in `getenv()`, ahead of what `putenv()` set,
  and in `getenv()` with no name and `$_ENV`. A script that changes
  `$_SERVER` does not change them, and `getenv($name, true)` skips them.
- Classic mode is faster: a request on the classic example's real page (two
  MySQL queries) went from 2,566 to 5,383 requests a second. A classic
  request runs on its thread's own stack instead of a fiber; its thread
  reads the environment once, keeps what Go tells it at start, checks a
  declaration of `scripts()` routes only once, and PHP no longer copies the
  environment into `$_SERVER` at each request only for Cairo to take it out
  again (`$_SERVER` is the same). In a classic script PHP's
  own waits (PDO, curl, `sleep()`) hold the thread, as under PHP-FPM; a
  request's deadline still ends a wait on a socket (PDO MySQL, streams) and
  a sleep, but no longer a curl transfer or a PostgreSQL query.
  `Cairo\async()` and the rest of the concurrency API work as before.
- In isolated mode `$_SERVER` keeps what the app put there as its thread
  started, under the request's own keys. Before, it had the request's keys
  only, and Symfony's `APP_ENV` or a variable of `.env` was gone from it
  after the first request. The process's environment stays out of it, as
  before (`getenv()` has it).
- `cairo serve --watch` with no `--watch-path` looks at the directory above
  the server file when that one has the `composer.json` and the server
  file's does not, as for `public/index.php`.
- Cairo is built with Go 1.27 (it was 1.24), and building it from source
  needs Go 1.27. With it come the defaults of Go 1.25 to 1.27. The one an
  operator may notice: in a container with a CPU limit, Go now counts the
  CPUs of the limit and no longer those of the machine. Under a limit of
  one CPU, the count that Go 1.24 arrives at on a machine with 64 CPUs
  served 11% fewer requests. The PHP threads are counted as before:
  `$app->threads()`, or one for each CPU of the machine. `GOMAXPROCS`
  still overrides Go's count. The static binary is built on Alpine 3.24
  (it was 3.21).
- In isolated mode a thread that answers gets its next request sooner: the
  goroutine that takes the answer gives the thread to the next request at
  once, and that request's event is ready before it waits. A plain PHP
  request with two MySQL queries serves 6.5% more requests a second, a
  hello world 10% more.

### Fixed

- A reload (`--watch`, SIGUSR1) serves the new code when php.ini has
  `opcache.validate_timestamps=0`, as production setups often do. opcache
  keeps no time for a file it compiles with that setting, so the new threads
  that a reload starts could not tell an old file from a new one and ran
  the old code, in every concurrency mode. Now, where php.ini turns the
  check off, each PHP thread turns it on with an `opcache.revalidate_freq`
  of ten years: opcache records the time of each file and compares it only
  on the threads of a reload, so without one it still never looks at the
  disk, and no request pays for it.
- The offload threads that a reload starts anew knew nothing of the reload
  (they kept what Cairo had told them at start), so with a PHP older than
  8.4, or a php.ini whose opcache looks at the disk only now and then,
  `offload()` could run the old code after a reload.
- `move_uploaded_file()` and `is_uploaded_file()` work again on the uploads
  of a classic script: since classic requests run on their thread's own
  stack, both fell back to PHP's own list of uploads, which is empty, so
  `move_uploaded_file()` returned false (WordPress' media uploads failed).
- A thread that `maxRequests()` or `maxMemory()` recycled, while the code on
  disk did not start (a parse error, other routes in `server.php`), had a
  replacement that died again and again, and after five tries the server
  stopped. Now the old thread stays in its place and serves on, and the
  replacement is tried again after 1 s, 2 s, 4 s and so on, a minute at
  most, with `php thread not replaced` in the log.
- A server no longer dies when opcache restarts its cache. opcache does that
  once the cache is full of stale code (files that changed under a running
  server) or after `opcache_reset()`, at the start of the next PHP request,
  which here is the next PHP thread that starts: a recycled one
  (`maxRequests()`, `maxMemory()`) or one that takes a dead one's place. It
  then wiped the cache under the threads that were running code from it, and
  the process ended with `zend_mm_heap corrupted`. Now no thread starts once
  opcache has scheduled a restart: the server finishes its requests and
  starts anew in place, under the same process id, with the reason in the
  log (`restarting in place`). Connections are refused until the new process
  listens.
  This needs PHP 8.4 or newer, where opcache tells of the restart; with an
  older PHP the server still dies.
- `cairo serve` no longer leaves its directory of runtime files
  (`cairo-runtime-*` in the temporary directory) behind when it stops.
- A PHP thread that died of a fatal error left the stacks of the fibers that
  were waiting on it behind: two memory mappings for each, and the memory
  they had touched. A server that lost threads while it held thousands of
  requests ran into the system's limit of mappings, and then no fiber could
  start (`Fiber stack protect failed`). The stacks are freed when the thread
  ends.

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

[Unreleased]: https://github.com/cairophp/cairo/compare/v0.6.0...HEAD
[0.6.0]: https://github.com/cairophp/cairo/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/cairophp/cairo/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/cairophp/cairo/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/cairophp/cairo/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/cairophp/cairo/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/cairophp/cairo/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/cairophp/cairo/releases/tag/v0.1.0
