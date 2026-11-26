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
| `cmd/cairo/` | The CLI: `serve` (with `--watch`), `php`, `gen`, `version`. Its blank import of `internal/sapi` is what links the extensions in. `php.go` reads the arguments of PHP's command line and decides when the binary is one: `cairo php`, the name `php`, or PHP's own arguments (how PHP code starts `PHP_BINARY` again). `phpbinary.go` hands the command to another PHP when `CAIRO_PHP_CLI` names one: the process becomes it |
| `internal/core/` | The core engine. cgo: the embedded PHP (`cairo.c`, Cairo's own SAPI), PHP threads, the goroutines of Go operations, the offload pool, what fibers wait on and the op registry. What it exports is all that an extension can use, and it names no extension: `boundary_test.go` fails when it does |
| `internal/core/engine.go`, `dispatch.go`, `supervise.go` | The engine in Go: start and shutdown, a request's way to a PHP thread, thread supervision |
| `internal/core/reload.go` | `Engine.Reload()`: new code for a running server. A PHP thread holds the code it has loaded, so new code needs new threads, and each request thread is replaced the way one is recycled (`replace` and `swap` in `supervise.go`; a replacement that ends before its loop takes no slot, and the old thread stays and is tried again later, `keep`): the new one runs `server.php` and comes to its loop, takes the pool slot, and the old one finishes what it holds. The first new thread is a trial with no slot: when it ends before its loop (a parse error, an exception) nothing is replaced, and when it builds another declaration than the servers were made from (`differs`, set in `goCairoSpec`; it then ends as `exit()` would) the reload ends with `ErrDeclarationChanged`. The others follow a quarter of the threads at a time, and the offload threads end after their jobs and start anew under their ids (`renewing`). A slot whose thread is being recycled just then is waited for (`claim`). Threads started after the first reload have opcache compare its files with the disk, whatever `php.ini` says (`reloaded` in `runtime.php`; where `php.ini` turns `opcache.validate_timestamps` off, every thread turns it on with a `revalidate_freq` of ten years before its first PHP request, because opcache records no time for a file compiled without it: `opcache_times.c`, which sets the thread's own value as PHP-FPM applies `php_admin_value`, so no request pays for it; before PHP 8.4, where the start gate cannot see opcache's restart coming, they compile for themselves and leave opcache alone); nothing calls `opcache_reset()`, which would empty the cache under the threads that run (see `startgate.c`). When opcache asks for its restart while the trial reads the code, the reload ends with `ErrRestartNeeded` |
| `internal/core/classic.c`, `classic.h`, `internal/httpserver/scripts.go`, `internal/sapi/http_input.c`, `internal/sapi/scripts.go`, `php/scripts.php` | Classic mode (`concurrency('classic')`): a PHP request for each HTTP request, as under PHP-FPM, for apps that are a set of scripts. In classic mode a request thread runs one PHP request per request on the same system thread: `cairo_thread_run()` in `cairo.c` loops while the request that ended asked for another (`cairo_thread_again()`), because a new system thread for each request measured 4 times the cost of the request (PHP copies its function, class and constant tables into each new thread). Between two PHP requests `cairo_request_ended()` forgets what the core and the extensions keep in `__thread` variables that point into the request's memory (`*_request_end()`, and the `request_end` of each `cairo_extension`). The scheduler takes one call and then has the loop leave the others in the inbox (`cairo_hold_calls()`, `holding` in `loop.c`) until the thread's next PHP request. That request runs on the thread's own stack, not in a fiber, whose stack would be mapped and unmapped for each one: `cairo_fiber_bind(null, ...)` binds its context to PHP's main fiber context, and what would suspend a fiber there runs the scheduler's loop until it is done (`Scheduler::onStack()`, `drive()`; `select()`, `offload()`, a pool and `Cairo\sleep()` wait in an `async()` task). PHP's own waits block the thread there, as under PHP-FPM, but the hooks end a socket's wait and a sleep at the request's deadline (`cairo_stack_deadline()`, which `cairo_fiber_bind()` sets). `cairo_run_script()` runs a script as PHP runs a request's: at the bottom of the stack (its variables are globals), `exit()` is the end of the script, and shutdown functions run before the answer. `$http->scripts($dir)` is a fallback route whose handler in Go (`scripts.go`, through an `os.Root` of the directory) names the script, and `php/scripts.php` sets `$_SERVER` for it, gives it `php://input` (`cairo_http_input()`) and runs it. Its output is the answer through `HttpOutput::classic()`, held until 1 MiB so that a script that closes every output buffer (WordPress at shutdown) is not streamed |
| `internal/core/cserve.c`, `cserve.h`, `internal/sapi/plain_request.c`, `plain_answer.c`, `plainstream.go`, `php/plain.php`, `php/scripts_in_c.php`, `php/plain_request.php`, `internal/core/php/loader.php` | Classic mode with no declaration (`cairo serve` on a folder or a plain file), and a declared classic app whose every route is `scripts()` (`ScriptsOnly` in `internal/spec`), serve each request as PHP's own SAPI request, as PHP-FPM does, with no Cairo PHP and no PHP of the app around the script: Cairo's PHP code around a script measured 39 µs of a hello's request, and a declared app's PHP 135 µs. An extension can serve a thread's requests in C (`cairo_cserver` in `extension.h`): the thread's first PHP request runs `php/plain.php`, or for a declaration a starter of the scheduler (`php/scripts_in_c.php`, after the declaration's check, and `Scheduler::leave()`), which calls `cairo_cserve()`, and from then on the thread's loop in `cairo.c` takes each call itself (`cairo_cserve_take()`, `cairo_loop_take_call()` in `loop.c`) before `php_request_startup()`, and the SAPI module's functions (`read_post`, `read_cookies`, `register_variables`, `ub_write`, `flush`, `send_headers`) are the server's while it runs one. PHP 8.4 and older have no `pre_request_init`, so `begin` sets `SG(request_info)` before the request starts, as FPM does; PHP itself then reads the cookies and parses the forms and uploads (Go keeps the body of such a scripts route raw: `RawBody`, set from `ScriptsOnly`). `plain_request.c` fills `$_SERVER` with what `php/scripts.php` sets for a declared app, runs the script with `php_execute_script()` and gives `getenv()` the CGI variables (`cairo_cgi_env_from()`). `plain_answer.c` holds the answer until it ends or reaches 1 MiB, then streams it from the thread (`goPlainOpen`, `goPlainSend` in `plainstream.go`, under the call's context: a slow client holds the thread back, as under FPM), and has `fastcgi_finish_request()` and `headers_send()` in C, which forward to `HttpOutput` where no script runs in C. A thread started after a reload has opcache compare its files with the disk, as `runtime.php` has it (`see_new_code()`). Past the deadline a script is stopped at its next safe point (`cairo_should_stop()` checks the stack deadline when the runtime is not loaded) and answered with a 504. Cairo's concurrency functions are there before the script runs (`php/plain_request.php`, 1.5 µs a request); the first use of a `Cairo\` class loads the runtime (`loader.php`, shared with `runtime.php`) and gives the request a context and a scheduler on its stack (`PlainRequest::upgrade()`), as a declared classic request has |
| `internal/core/startgate.c`, `startgate.h` | A PHP thread that starts while others run, and opcache. opcache restarts its cache (it is full of stale code, or the app called `opcache_reset()`) at the start of the next PHP request, once the file locks of its requests tell it that nobody runs cached code. Those locks are a process's, so in a process of threads it finds nobody and wipes the cache under the threads that run. A thread here is one PHP request for its life, so that next request is the next thread to start (in classic mode a thread starts a PHP request for each request, and passes the gate before each one). opcache tells of a restart it schedules through `zend_accel_schedule_restart_hook` (PHP 8.4 and newer; with an older PHP no thread is refused and the crash stays): from then on `cairo_startgate_startup()` lets no thread start (a mutex makes the two exclusive), the thread ends with `CAIRO_START_REFUSED`, the engine reports `ErrRestartNeeded`, and `cmd/cairo` stops the server and starts the process anew in place (`restart.go`) |
| `internal/core/cli.c`, `cli.h`, `cli.go`, `module.h` | PHP's command line (`cairo php`, `RunCLI`), which follows PHP's own `sapi/cli`: a second SAPI module, named `cli`, that shares the cairo module with the server's (`module.h`), so the extensions and their hooks are there. One thread, made in C like a PHP thread, runs one script, and no server and no other thread starts. That thread is PHP thread 0 of the engine and the script is the one request it ever takes (`hostScript`: a call that Go holds, under which the operations of the script's fibers run): `runtime.php` loads Cairo's classes (mode `cli`) and its scheduler starts the script in a fiber (`Scheduler::script()`), which runs it with `cairo_cli_script()`, at the bottom of its own stack: its variables are the globals. Cairo's files are removed right then (`goCLILoaded`). The loop ends with the script and the fibers it started, or when nothing is left that could wake it (`cairo_stuck()`). While the script has one fiber to run, a wait is the thread's, as under PHP (`Scheduler::waitsAlone()`), whatever a fiber waits for aside (`callAside()`). `CAIRO_SCHEDULER=off` runs the script outside any fiber and with no scheduler, for a program with an event loop of its own for fibers |
| `internal/core/operation.go`, `opworker.go`, `waitops.go`, `waitops.c`, `php/context.php` | What a fiber asks Go for. A fiber has no goroutine and no context in Go: `async()` and the derived contexts (`withTimeout()`, `withCancel()`) are PHP alone. An operation names its request and its scope (`Context::goScope()`: nothing, a deadline, or cancelable), `submit` runs it under the request's context, or under one made for it when it has a scope, on a goroutine that then waits for the thread's next operation, and the result is an event in the thread's inbox. `Context::cancel()` ends the operations in flight under it with `cairo_op_cancel()`. A request that PHP has answered has no context in Go any more, and what runs on after it (`Context::defer()`) submits with the request's sign turned: such an operation runs under no context but its scope |
| `internal/core/loop.c`, `loop.h`, `sockets.c`, `sockets.h`, `mailbox.go`, `php/waits.php` | The loop of a PHP thread and the waits that stay on it. `cairo_next_event()` is where the thread comes back between fibers: it wakes the parked fibers whose time is up and those whose sockets are ready, takes the events Go queued for it (its inbox) and, with none and no fiber to run, sleeps, in C and without a call into Go: on a futex, or in its epoll while a fiber waits on a socket; when it gave Go an operation or an answer on its way, it first watches the inbox for a few microseconds (`look`). The mailbox is the memory both sides read and write without a call: the inbox, how many events were queued and taken, and since when the thread runs PHP (0 while it sleeps), with when it last went on from one ready fiber to the next (`cairo_loop_resumes()`). Send to an inbox with `thread.send()`, which wakes the thread when it sleeps. `Waits` parks the fibers in `sleep()`, in `select()` and on a socket (`Waits::socket()`, behind the hooks of PHP's streams, PDO MySQL and curl) on the thread: it says until when and under which context, and C keeps the fiber and its timer (`parked.c`, below); no goroutine is involved, and a context that ends wakes them from PHP (`Waits::end()`). Each thread has an epoll of its own (`sockets.c`): one call arms a fiber's socket there and parks the fiber (`cairo_wait_socket()`), the loop wakes it once the socket is ready, without running PHP for it, and Go wakes a thread that sleeps in the epoll by writing to the thread's eventfd |
| `internal/core/php/scheduler.php`, `fibers.c`, `fibers.h`, `parked.c`, `parked.h` | The scheduler of a PHP thread. PHP decides and C keeps and switches. The loop in `scheduler.php` resumes the fibers that are ready (`cairo_run_ready()`), then takes events (`cairo_next_event()`); a request starts in a fiber, which answers it with the `Answer` of its server and ends it (`Scheduler::serve()`). `fibers.c` has the context each fiber runs under, in a reserved pointer of the fiber (`cairo_fiber_bind()`; `Context::current()` reads it), the fibers that wait for their next job instead of ending (`cairo_fiber_idle()`, `cairo_fiber_reuse()`; 64 at most) and the queue of those to resume. `parked.c` has the waits and their timers: a wait is a struct on the stack of the fiber that waits (`cairo_park()`; `cairo_park_fiber()` for a wait that begins in C), found by its id (`cairo_wake()`) and by its request (`cairo_parked_under()`). All of it is the memory of the thread's PHP request, so it counts against the thread's memory limits. PHP below 8.4 has no `zend_fiber_resume()` to call: there `fibers.c` switches with its own copy of it. A thread that a fatal error ends frees the stacks of its suspended fibers itself (`cairo_fibers_shutdown()`, at request shutdown): PHP frees a stack only when its fiber ends, and ends none then |
| `internal/core/queue.h`, `outbox.c`, `outbox.h`, `outbox.go` | The queues between Go and the PHP threads, and the way of an answer. A message is C memory that its sender pushes without a lock and its one receiver takes all at once (`queue.h`): a thread's inbox is one such queue, and the outbox is the one that all threads share for what they have for Go. `cairo_answer()` (declared in `extension.h`) queues the answer to a call there and the thread goes on, without running Go code; one goroutine (`collect`) takes the answers and hands each to the goroutine in `Dispatch` that waits for it, and learns there that a thread has come to its loop for the first time (`cairo_outbox_ready()`: a recycled thread serves until its replacement has) and of a thread that has ended. A body of 32 KiB or more is not copied for Go: its message leaves the list that `collect` frees and belongs to the call, `Engine.DispatchLent` returns the body as a view of it with a function that frees it (the HTTP server writes it to its client and releases it), and `Engine.Dispatch` copies it. The calls a thread holds and the contexts made for its operations are in tables under the thread's mutex, which that goroutine, dispatch and the thread's own calls into Go share |
| `internal/core/offload.c`, `offload.h`, `offload.go`, `php/offload.php` | The way of an `offload()` job, which Go has no part in. The request thread serializes the job and queues it straight into the inbox of the offload thread with the least to do (`cairo_offload_send()`), its fiber parks on the thread (`Jobs`, under its context like any wait there), and the offload thread queues the serialized result straight into the inbox of the request thread (`cairo_offload_done()`): two wake-ups of threads. Job ids are the process's, so a result that comes after its fiber stopped waiting, or for a thread that was replaced, finds nobody. Go names the offload threads at start and, for one that died in the middle of a job, tells the fiber that waits (`jobLost`) |
| `internal/core/goops.go` | The op registry: `core.RegisterOp("name", fn)` in Go, `Cairo\Internal\Go::call('name', $payload)` in PHP. The payload array goes to Go in `internal/wire`'s binary format, where `wire.Unmarshal` reads it into a struct by its `json` tags; results come back as events. `RegisterFiberOp` is for what PHP's hooked functions ask for |
| `internal/core/extension.go`, `extension.h`, `extension.c` | How an extension's C code joins the SAPI module: it defines a `cairo_extension` (what to run at module startup, as a PHP request shuts down, when a PHP thread ends and when a thread that the extension interrupted reaches a safe point, what takes the output that a server's PHP writes outside every buffer and its `flush()`, the PHP functions it adds) and its Go file adds it with `core.RegisterC` from `init()`. C types do not cross Go packages, so `internal/sapi` has a `registerC` that takes its own |
| `internal/wire/` | The binary format of the events Go sends to PHP threads (requests, op results), which `internal/core/wire.c` decodes into PHP arrays, and of the arguments PHP passes to a Go operation, which `wire.c` encodes and `wire.Unmarshal` decodes. Bodies cross as raw bytes, and a value that something else reads or wrote crosses as it is (`wire.Raw`). An event may end in raw bytes that it leaves out (`wire.TailEvent`, the body of an HTTP request): the engine copies them behind the event, straight into the thread's message (`cairo_inbox_call()`) |
| `internal/core/php/` | The core's PHP files, one per concern, embedded in the binary. Every PHP thread runs `runtime.php`, which loads the core's other files (`app.php`, `context.php`, `channel.php`, `async.php`, `offload.php`, `waits.php`, `scheduler.php`, `go.php`; `request_local.php` on first use) and then the files of the extensions |
| `internal/core/phpfiles.go` | How an extension's PHP file joins the runtime: `core.RegisterPHP(files, "name.php")` from its Go file's `init()` (`registerPHP("name.php")` in `internal/sapi`), loaded at start, or on first use of the classes it names. The engine writes the files of the core and of every extension into one directory when it starts, so a file name can be taken once |
| `internal/core/onstart.go`, `probe.go` | What an extension runs when the engine starts (`RegisterStart`), and how it follows requests through the engine: `ProbeRequests` gives each request a `Probe`, which is told of the waits of its fibers and of the stretches of PHP its thread ran. The engine measures nothing itself. `internal/sapi/timing.go` is the one that asks: the request timing behind the slow log (`slowlog.go`) and the heavy routes (`heavy.go`) |
| `internal/sapi/` | The extensions that are cgo or Go operations: the runtime hooks, the PDO pgsql driver, the clients, the diagnostics, how an HTTP answer leaves PHP. Every file joins the core from its `init()` |
| `php/` | The PHP files of the extensions, one per concern, embedded in the binary |
| `internal/sapi/env_hook.c`, `env_hook.h`, `env.go` | `putenv()` and `getenv()` on the PHP request's own environment (each request's in classic mode, the thread's otherwise): PHP's `putenv()` writes the process's environment, which libc's `getenv()` reads with no lock, PHP's own request start included, so a `putenv()` (or the `unsetenv()` PHP undoes it with at request end) on one thread crashed another that read. `putenv()` here writes a table in the request's memory, which `getenv()` reads first and `getenv()` with no name lays over the process's; the process's environment is only read. A classic script's CGI variables come before that table, as under PHP-FPM: `php/scripts.php` hands its `$_SERVER` to `cairo_cgi_env()` just before the script runs, and C keeps a reference to that array (a write to `$_SERVER` then separates the script's from it), which `getenv()` and `$_ENV` (`php_import_environment_variables`) read. Both are freed in the extension's `request_shutdown`, while the request's memory is still there |
| `internal/sapi/blocking.go`, `blocking.c`, `php/blocking.php` | The blocking detector, which watches threads instead of requests: it asks the engine which threads are running PHP and since when each runs the fiber it is in (`EachBusyThread`), and interrupts one that has for too long (`InterruptThread`), and that thread reports from the `interrupt` of the detector's `cairo_extension`. Its goroutine ends when the engine does (`Stopping`) |
| `internal/sapi/signals.c`, `signals.h`, `signals.go`, `php/signals.php` | Signals for the scripts of `cairo php`, with PHP's own pcntl (compiled in by `scripts/build-php.sh`). A signal for the process lands on any thread, most often one of Go's, where PHP's handler has nobody to record it for. So for a signal the script handles, the handler the system calls is `cairo_signal_handler()`, put in front of PHP's around `pcntl_signal()`: it gives the signal to Go wherever it landed, `os/signal` queues it, and `deliverSignals` brings it to the script's thread (`cairo_signal_deliver()`). Under the scheduler the signal is marked pending, the thread is interrupted (`cairo_vm_interrupt()`) and kicked out of a system call it may be in; the fiber that runs, at its next safe point (the extension's `interrupt`), or else the fiber that `Cairo\Internal\Signals` keeps for it, raises the signal on its own thread, where it now goes to PHP's handler, and pcntl runs the script's handler in that fiber. No handler runs inside Cairo's own PHP code (`Signals::interruptible()`): the signal stays pending there, and `deliverSignals` interrupts the thread again every millisecond until a fiber took it, because an interrupt asked for from inside the interrupt comes back before the code has moved on. pcntl lets no fiber switch inside a handler, so a handler that waits is refused with a `FiberError` (`cairo_fiber_call()` refuses before it submits) unless the script waits with its thread. That fiber waits in Go aside (`Scheduler::callAside()`): it does not keep a script with one fiber from waiting with its thread. With `CAIRO_SCHEDULER=off` the kick itself goes to PHP's handler. No signal is ever blocked on the thread: a mask would be inherited by the processes the script starts. A server has none of pcntl's functions, and nothing has those that fork or wait for a signal with the thread |
| `php/http_answer.php`, `php/grpc_answer.php` | How a request of each server is answered. The scheduler knows no server: a server's PHP file gives it an `Answer` with `Scheduler::answerWith()`, which answers a request in the request's fiber and names its route for the logs. `internal/sapi/http_answer.c` has `cairo_respond_http()`, which sends an HTTP answer to Go (`cairo_answer()`). The form its status and headers cross in is the HTTP server's own: written there, read by `decodeAnswer` in `internal/httpserver/answer.go`. `internal/sapi/grpc_answer.c` has `cairo_respond_grpc()` for a gRPC call, read by `answer` in `internal/grpcserver/server.go`. A reply to a stream does not wait for the method to end: `cairo_grpc_send()` in `internal/sapi/grpc_stream.c` puts it in the stream's queue in Go with one call (`goGrpcSend` in `grpcsend.go`) |
| `internal/filewatch/`, `cmd/cairo/reload.go`, `restart.go` | `cairo serve --watch` and the reload signal. `filewatch` tells when the code of an app changes by looking: it reads the times and sizes of the files again every 250 ms (`*.php`, `*.twig`, `*.yaml`, `*.yml`, `.env*`, `composer.lock`; not `vendor`, `node_modules`, `storage`, `var` or what begins with a dot) and reports once they have stood still for 100 ms. No file events of the kernel: they do not arrive for a directory shared with a container, and need a module. `reload.go` calls `Engine.Reload()` for a change and for SIGUSR1. A changed declaration needs new servers, and a changed `.env` a new process (a library that reads it leaves alone what the environment has already), so for those `--watch` stops the server and starts the process anew in place (`restartProcess`, an `exec`: the same process id); the signal never restarts: it refuses another declaration and says so. With no `--watch-path`, `--watch` looks at the server file's directory, or at the one above it when only that one has a `composer.json` (`appDir`, for `public/index.php`) |
| `internal/duplex/` | The two ends of a stream as PHP has them: of a gRPC stream, for a method it serves and for a call it makes as a client (`internal/sapi/grpcstream.go`), the sending end of a streamed HTTP answer, and both ends of a WebSocket. What PHP sends waits in a `SendQueue` for the one goroutine that writes: 64 messages at most, put there by a direct call from the PHP thread, or by a Go operation that waits when it is full. What the other side sends is read ahead of PHP by a `ReadAhead`, 64 messages or 1 MiB at most, and one `recv` operation takes all that has arrived; a WebSocket takes them without an operation (`TryTake`) and waits in one only when none has arrived (`Wait`) |
| `internal/httpserver/stream.go`, `bodystream.go`, `sendfile.go`, `internal/sapi/httpstream.go`, `http_stream.c`, `php/http_stream.php` | An HTTP answer that is sent while its handler runs, and what else PHP does with its client during a request. The request's event is the context of its call (`event.Value`), so a Go operation finds the client it answers with no table (`exchangeOf`; a cached route has none, its answer is shared). `http.stream.open` writes the head and keeps the stream under an id, each piece is queued with one direct call (`cairo_http_send()`, `goHTTPSend`) into a `duplex.SendQueue` of 64, and `http.stream.write` waits for room there: a slow client holds its fiber back. The call stays open meanwhile; when PHP answers it, the handler in Go closes the queue, and cuts the connection for a handler that failed after the head (`stream.finish`). The server's write timeout bounds each write of a stream, not the stream. A client that leaves ends the handler in Go while PHP may still run: an operation that writes takes the event's lock and is refused after `event.end()`. A server that stops refuses each stream its next piece (`stopStreams`), which ends the handler as a client that left does. `Hint` sends 103 Early Hints. `bodystream.go` is the other direction: a route with `streamBody()` gets no body, and `http.body.read` reads the next piece from the connection (full duplex on HTTP/1, no read deadline). `sendfile.go` wraps the routes when `$http->sendfile()` named directories: an answer whose head names a file (`X-Sendfile`, or `X-Accel-Redirect` under a prefix) is answered with the file by `http.ServeContent`, through an `os.Root` of the directory, and the request's own `X-Sendfile-Type` and `X-Accel-Mapping` are replaced so that a framework which trusts them can. `http_stream.c` is how echo becomes an answer: it takes the output of a fiber that asked (`cairo_http_capture()`, the `output` and `flush` of its `cairo_extension`) and calls `Cairo\Internal\HttpOutput::drain()` at `flush()` and every 8 KiB; `HttpOutput` is that for classic PHP in isolated mode (`classic()`, with `headers_send()` and `fastcgi_finish_request()`) and for a framework's streamed response (`send()`, used by `Http\Adapter`) |
| `internal/httpserver/websocket.go`, `topics.go`, `internal/sapi/websocket.go`, `websocket.c`, `php/websocket.php` | WebSockets that PHP serves, and topics. A stream, a WebSocket and the events of topics are what PHP can make of a request's client instead of answering (`began` in `stream.go`): each begins in a Go operation while the call is open, and the handler in Go ends it when the call returns (`finish`). The handshake is such an operation (`http.ws.open`, `OpenSocket`), so the route's handler has agreed first; the library (`github.com/coder/websocket`) checks the Origin and answers a refused handshake itself. A message crosses with its type in its first byte. PHP sends with one direct call into a `duplex.SendQueue` (`cairo_ws_send()`, `goWSSend`) and takes what a `duplex.ReadAhead` read ahead with another (`cairo_ws_take()`, `goWSTake`); the operations `http.ws.send` and `http.ws.wait` are for a full queue and for nothing to take. A client that leaves does not end the call, as the client of a stream does: what it sent stays PHP's to take, and PHP learns of the end from `receive()` and `send()`. Topics are the process's (`topics.go`): `Topic::publish()` is one direct call (`cairo_topic_publish()`, `goTopicPublish`) into a queue of 1024 that one goroutine hands out in order (`http.topics.publish` waits when it is full), to the sockets that subscribed and to the answers that Go serves alone (`topicsAnswer`, from `Response::eventsFrom()`: PHP names the topics in `http.topics.listen` and its fiber ends). Handing out waits for nobody: a listener whose queue of 64 is full is dropped. A server that stops closes its sockets with 1001 and ends the answers it serves (`stopSockets`, `stopTopics`) |
| `php/leak_watch.php` | How a diagnostic joins the scheduler, which names none: `Scheduler::onStart()` runs it on each request thread that starts, and there it asks to be called after each request (`afterRequest()`) when the app turned it on. The detector itself (`leaks.php`) loads on first use |
| `php/cache.php`, `php/pool.php` | `Cairo\Cache` and `Cairo\Pool`, each loaded on first use |
| `cairophp/symfony-runtime` (its own repository) | A Symfony app served through its own front controller (`cairo serve public/index.php`), with no server.php, as `runtime/frankenphp-symfony` does for FrankenPHP. Symfony's `autoload_runtime.php` takes the runtime class from `extra.runtime.class` or `APP_RUNTIME` and ends the script with `exit()` once the runner returns: the package's runner declares the HTTP server for the kernel and serves it from inside `run()` with `App::serve()` (`serve()` in `runtime.php`, for a file that never returns its App), and `run_file()` in `cairo.c` counts that `exit(0)` as a clean end. `examples/serve` and `test/serve_test.go` keep that path working here; the package's own tests serve a Symfony app with a `cairo` binary |
| `internal/spec/` | The declaration that `server.php` sends to Go. This package reads the core's part (threads, the concurrency mode, limits) and keeps what each extension declared as PHP sent it, under the extension's name. `Claims` is what servers must not share: listen addresses and handler ids |
| `internal/httpserver/`, `internal/grpcserver/` | net/http and grpc-go servers built from the declaration. Each reads and checks its own part of it (`declaration.go`); `cmd/cairo` gives both to the engine to run in the declare phase |
| `internal/pbwire/` | A gRPC message between protobuf's bytes and the array PHP has for it, in `internal/wire`'s form, with no message object or JSON text in between. The array is protojson's, and whatever it does not convert exactly as protojson would (a google.protobuf type, an unusual value, every error) it leaves to `grpcserver/message.go`, which goes through protojson. Its tests compare both directions with protojson on random and damaged messages, and `test/grpc_kinds_test.go` has PHP compare with `json_decode()` |
| `internal/middleware/` | Go middleware declared from PHP |
| `internal/servertls/` | The TLS side of a declared server, for `httpserver` and `grpcserver`: a certificate from two files, read again when they change, or certificates from an ACME authority (`golang.org/x/crypto/acme/autocert`) |
| `internal/telemetry/` | What the process reports of itself: the metrics behind `$http->metrics()`, the traces of `$app->tracing()`, and whether it is ready (`$http->health()`). The servers ask it to wrap their router (`Telemetry.HTTP`) and for their gRPC options (`GRPCServer`, and `GRPCClient` for the clients PHP opens); with nothing declared it wraps nothing. The metrics are Prometheus' library with one histogram per request, by hand (`metrics.go`): OpenTelemetry's instrumentation measured 11 µs of CPU per request for the same. The traces are OpenTelemetry's SDK, which samples, batches and exports. The span of an HTTP request is made by hand on it (`http.go`), because OpenTelemetry's instrumentation of net/http measured 20 µs of CPU for every request, the ones that are not sampled too; the spans of gRPC are its instrumentation of grpc-go. A label or a span's name never comes from what a client can choose freely: the route is the router's pattern, a method outside HTTP's is `other`, an unknown gRPC method is not followed, a span has no Host header, and the values of a span that are the client's (the path, the user agent) are cut at 2048 bytes. `internal/httpserver/ops.go` has the endpoints Go answers itself, and `Engine.Stats()` (`internal/core/stats.go`) is what the metrics read of the PHP threads at each scrape |
| `internal/mercurehub/`, `internal/sapi/mercure.go`, `php/mercure.php`, `php/mercure_publish.php` | The Mercure hub of `$http->mercure()`. `internal/mercurehub` is the one package that imports the hub (`github.com/dunglas/mercure`, **AGPL-3.0**: see `third_party/NOTICES`), and the build tag `nomercure` leaves it out (`nohub.go`): import the hub nowhere else. The HTTP server gives the hub its requests before its own router (`Hub.Routes`), outside the metrics, the traces and the timeout, because a subscriber stays for hours and holds no PHP. `mercure_publish()` is a Go operation that calls `Hub.Publish`. Clients from before Mercure 1.0 need the hub's build tags `deprecated_topic` and `deprecated_claim`, which the Makefile and the static build set (`TAGS`, `CAIRO_TAGS`) |
| `internal/cachetag/` | Tags of cached responses: declared by routes in `httpserver`, purged from PHP through `sapi` |
| `internal/codegen/` | `cairo gen`: PHP classes and clients from `.proto` files |
| `stubs/` | Generated IDE stubs of the public API in `internal/core/php/` and `php/`; `scripts/publish-stubs.sh` copies this directory to `cairophp/stubs`, the Composer package, on each release |
| `examples/` | Runnable apps. The end-to-end tests serve these, so they are also fixtures |
| `test/` | End-to-end tests: they start the `cairo` binary on an example and talk to it over HTTP and gRPC |
| `scripts/` | `build-php.sh` (ZTS PHP into /opt/php-zts), `build-static.sh` (the static binary), `stubs.php`, `publish-stubs.sh`, `core-names.php` (what the core's PHP files name of the extensions, for the boundary test) |
| `Dockerfile.native`, `scripts/php-image.sh`, `test/native-image/` | The native image: Cairo built against the libphp.so of the official thread-safe `php` image of a PHP version (`php-image.sh` names it: `php:8.4-zts-trixie`, from 8.6 on `php:8.6-cli-trixie`), so an app adds extensions with that image's tools and Cairo loads them. The static binary cannot load any (musl's static programs have no `dlopen`): `Dockerfile.static` builds more in (`EXTRA_EXTENSIONS`, `PECL_EXTENSIONS`, `APK_PACKAGES`). CI builds each native image and a static binary with APCu and shmop, and checks that they load |
| `.github/php-versions.json`, `scripts/php-patches/` | The PHP versions a release ships, newest last: CI builds and tests a static binary of each, the release publishes one per version and architecture (`cairo-php8.4-linux-amd64`, the newest also as `cairo-linux-amd64`) and an image per version (`x.y-php8.4`, `php8.4`; the newest is also `x.y` and `latest`). A new PHP is one entry there, and the default of `Dockerfile.static` and `build-static.sh`. Before 8.5 opcache can only be a shared extension, which a static binary cannot load: `build-static.sh` builds it in with static-php-cli's patches (`php-patches/static-opcache-8.x.patch`, which define `HAVE_OPCACHE`; `cairo.c` then knows it is built in) and leaves its JIT out |

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
| Extensions | Servers (HTTP, its response cache, middleware, gRPC), clients (`Grpc\Client`, `Cairo\Net`), databases (the PDO pgsql driver, `Cairo\Pool`), PHP hooks (streams, sleep, DNS, TLS, curl), bridges (PSR adapters), diagnostics (slow log, heavy routes, leaks, blocking), observability (metrics, probes, tracing), `cairo gen` |

The core is `internal/core` (Go and C) with its PHP files in
`internal/core/php`, and the two packages it imports, `internal/spec` and
`internal/wire`. Everything else is an extension: `internal/sapi` (those that
are cgo or Go operations) with their PHP files in `php/`, the servers in
`internal/httpserver` and `internal/grpcserver`, `internal/pbwire`,
`internal/middleware`, `internal/servertls`, `internal/cachetag`, `internal/telemetry` and
`internal/codegen`.

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
(`Engine.EachBusyThread`), which it can interrupt (`Engine.InterruptThread`, or `cairo_vm_interrupt()` from C),
the state of the threads for a report (`Engine.Stats`),
the engine's logger (`Logger`), and what a request thread runs when it starts
and after each request (`Scheduler::onStart()`, `afterRequest()`).

`Cairo\App` is the declaration's public face. `server.php` sees one class,
so it has a method for every setting, an extension's included, and it is
where PHP names the servers, as `cmd/cairo` is in Go. It does not look
inside what a server declared: that goes to Go under the extension's name,
and only the extension's Go package knows its shape. The settings of an
extension go the same way (`slowRequests()` and `heavyRoutes()` under
`timing`, `detectBlocking()` under `blocking`, `tracing()` under `tracing`).

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
| `CAIRO_TEST_NO_OPCACHE=1` | Do not require opcache (for a binary built without it) |
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
- **Per-thread state that points into a PHP request's memory must be
  forgotten when the request ends.** In classic mode one system thread runs
  many PHP requests in a row, and PHP frees a request's memory (`emalloc`,
  its objects, its fibers) when it ends. A `static __thread` pointer, table or
  count into that memory that is not reset in `cairo_request_ended()` (a
  `*_request_end()` function of the core, or the `request_end` of an
  extension's `cairo_extension`) makes the next request on that thread use
  freed memory: a crash or corrupted data, in classic mode only, which fiber
  and isolated mode never show. Add the reset in the same change as the
  variable, and run `test/classic_test.go`, which serves several requests on
  one thread.
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
- CI (`.github/workflows/ci.yml`) runs the suite on a native ZTS build
  (amd64) and against the static binary on amd64 and arm64, for each PHP of
  `.github/php-versions.json`, with MariaDB and PostgreSQL.

## Releasing

1. Move the Unreleased entries of `CHANGELOG.md` into a new
   `## [x.y.z] - YYYY-MM-DD` section and update the links at the bottom.
2. Merge that to `main`, then tag it: `git tag vx.y.z && git push origin vx.y.z`.
3. `.github/workflows/release.yml` builds the static binaries for amd64 and
   arm64 and each PHP of `.github/php-versions.json`, with `cairo version`
   set to the tag, checks them, and publishes a GitHub release with
   `SHA256SUMS` and the changelog section as notes. Tags with a hyphen
   (`v0.2.0-rc.1`) become prereleases. It then calls
   `.github/workflows/image.yml`, which puts those binaries on Alpine
   (`Dockerfile.image`) and pushes `ghcr.io/cairophp/cairo` tagged
   `x.y.z-phpA.B`, `x.y-phpA.B` and `phpA.B` for each PHP, and `x.y.z`, `x.y`
   and `latest` for the newest, and the native image of each PHP
   (`Dockerfile.native`) tagged the same with `-trixie` behind
   (`x.y-phpA.B-trixie`; `x.y-trixie` and `trixie` for the newest).
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
