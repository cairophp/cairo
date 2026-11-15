# Cairo

Cairo is a PHP application server written in Go. You declare HTTP and gRPC
servers in PHP; Go does the serving. Every request runs in its own PHP Fiber,
and every fiber has its own goroutine, so one PHP thread can hold thousands of
requests at once while Go does the waiting.

> Status: milestone 1 prototype plus the first milestone 2 hooks. Design doc:
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
    sleep(1);                    // this fiber suspends; its goroutine waits
    return Response::json(['ok' => true]);
});
$http->get('/report', fn () => Response::json(offload('buildReport', 2026)));  // CPU work, other threads

$grpc = $app->grpc(':9000')->proto(__DIR__ . '/proto/helloworld.proto');
$grpc->service('helloworld.Greeter', Greeter::class);

return $app;
```

```sh
cairo serve server.php
```

## How it works

1. **Declare.** `cairo serve server.php` runs the file once. `App`, `http()`,
   `get()`, `grpc()` and `service()` only record a description of the servers,
   which goes to Go.
2. **Serve.** Go opens the listeners, builds the router and compiles the
   `.proto` files (no `protoc` needed). It starts the PHP request threads and
   the offload threads; each runs `server.php` again to build its own handlers.
3. **One fiber, one goroutine.** Go accepts a request on a goroutine and sends
   it to the least busy request thread, where it starts in a new Fiber. That
   goroutine owns the request's `context.Context`. When the handler calls a
   Cairo I/O function such as `Cairo\sleep()`, the fiber suspends, the
   goroutine performs the operation, then wakes the fiber. `Cairo\async()`
   starts a child fiber with its own child goroutine and child context.
4. **CPU work.** `Cairo\offload('fn', ...$args)` runs a function on the offload
   pool, so a heavy computation never stalls the fibers on a request thread.
   Code cannot move between PHP threads, so pass a function name or
   `[Class::class, 'staticMethod']`, with serializable arguments.

### gRPC

Go decodes each call with the method's descriptor and gives PHP the message
as an array (protojson). The handler returns an array, which Go encodes:

```php
final class Greeter
{
    public function SayHello(array $req, Cairo\Context $ctx): array
    {
        if (($req['name'] ?? '') === '') {
            throw new Cairo\Grpc\Exception(Cairo\Grpc\Code::INVALID_ARGUMENT, 'name is required');
        }
        return ['message' => "Hello {$req['name']}"];
    }
}
```

Health checking (`grpc.health.v1`) and server reflection are built in, so
`grpcurl -plaintext localhost:9000 list` works. Generated PHP message classes
come in milestone 2.

### Context and deadlines

`Cairo\Context::current()` (also passed to handlers) exposes the call's
deadline: `remaining()`, `deadline()`, `err()`, `throwIfDone()`. A Cairo I/O
call that is still waiting when the deadline passes throws
`Cairo\Context\DeadlineExceeded`, which becomes `DEADLINE_EXCEEDED` for gRPC
and `504` for HTTP.

### Concurrency API

```php
use function Cairo\{async, await, all, sleep, offload};

[$a, $b] = await(all([
    async(fn () => fetchUser($id)),     // each async() fiber gets its own goroutine
    async(fn () => fetchOrders($id)),
]));
```

### Runtime hooks

Cairo swaps the handlers of some blocking PHP functions so that, inside a
request fiber, they suspend the fiber while its goroutine waits, and existing
code needs no changes. Outside request fibers (the declare phase, offload
threads, fibers your own code creates) the original functions run.

| Hook | Functions | Notes |
|---|---|---|
| `Hook::SLEEP` | `sleep()`, `usleep()`, `time_nanosleep()` | |
| `Hook::SOCKETS` | reads and writes on `tcp://` and `unix://` streams: mysqlnd (PDO MySQL, mysqli), Predis, phpredis, `fsockopen()`, `stream_socket_client()` | waits go through one Go epoll loop |
| `Hook::CONNECT` | connects on those streams, DNS included | Go's dialer; falls back to PHP when a `bindto` option is set |
| `Hook::DNS` | `gethostbyname()`, `gethostbynamel()` | Go's resolver |
| `Hook::CURL` | `curl_exec()` | libcurl still does the transfer; only its socket waits move to Go, so `curl_getinfo()`, `curl_error()` and callbacks are unchanged |

```php
$app->hooks(Hook::ALL);    // the default; Hook::NONE turns them off
```

A hooked call honours the request's deadline like `Cairo\sleep()` does.
Socket waits go through one Go epoll loop, so a thousand waiting queries do not
hold a thousand OS threads. With one PHP thread, 50 PDO queries running
`SELECT SLEEP(1)` finish in about 1 second (20 take 20 seconds with
`Hook::NONE`).

pdo_pgsql talks to libpq's own sockets, which no PHP-level hook sees; a
Go-backed PDO driver for Postgres is planned. TLS streams (`ssl://`, `tls://`)
are not hooked yet, and `curl_multi_*` keeps blocking in `curl_multi_select()`.

### Connection pools

Each fiber needs its own connection. `Cairo\Pool` keeps a pool per PHP thread
(build it in `server.php`, which every thread runs):

```php
$db = new Cairo\Pool(fn () => new PDO($dsn, $user, $pass), max: 10);

$pdo = $db->get();                                  // this fiber's, returned when it ends
$db->with(fn (PDO $pdo) => $pdo->query('SELECT 1')); // borrowed for the callback only
```

When all connections are in use, `get()` suspends the fiber until one comes
back. Using one connection from two fibers at once throws instead of
corrupting the protocol.

### Request state: Context values

Globals and static properties are shared by every fiber on a thread, so
request state goes on the context instead:

```php
use Cairo\Context;

Context::set('user', $user);                 // this fiber and the tasks it starts
Context::get('user');                        // null (or a default) when unset
Context::has('tenant');
Context::getOrSet('db', fn () => $pool->get());
Context::defer(fn () => $pool->put(Context::get('db')));   // when the fiber ends
```

Values follow Go's `context.WithValue`: an `async()` task sees its parent's
values, and a value it sets stays in the task. Deferred callbacks run last
first when the fiber ends; for a request, that is after the response is sent.

Rules that keep fibers safe: per-request data lives on `Request` and
`Context`, not in superglobals (Cairo does not fill `$_GET`/`$_SERVER`), and
blocking calls that are not hooked yet (pdo_pgsql, TLS streams,
`curl_multi_select()`) stall every fiber on their thread. Use `Cairo\offload()` for CPU-heavy work.

## Building

Cairo links against a thread-safe (ZTS) PHP built with the embed SAPI.

```sh
make php            # builds PHP 8.3 (ZTS, embed) into /opt/php-zts
make build          # bin/cairo
make test           # unit + end-to-end tests (set CAIRO_TEST_MYSQL to include PDO MySQL)
make run            # serves examples/greeter
```

Or with Docker: `docker build -t cairo . && docker run -p 8080:8080 -p 9000:9000 cairo`.

Requirements: Go 1.24+, a C toolchain, libcurl headers (`libcurl4-openssl-dev`), and PHP 8.1 or newer (fibers).

## Layout

```
cmd/cairo/            the binary
internal/sapi/        embedded PHP: C bridge, PHP threads, fiber goroutines, offload pool
internal/spec/        the server declaration sent by server.php
internal/httpserver/  net/http server built from the declaration
internal/grpcserver/  grpc-go server, proto compilation, health, reflection
php/runtime.php       the PHP side: Cairo\ classes and the fiber scheduler (embedded in the binary)
examples/greeter/     HTTP + gRPC example
test/                 end-to-end tests
```

## Not done yet

- Streaming gRPC, generated PHP message classes, a `cairo protoc` command.
- TLS stream hooks, `curl_multi` hooks, a Go-backed HTTP client, a pgx PDO driver.
- Hard interruption of PHP code at a deadline, `isolated` mode for non-fiber-safe apps.
- Opcache and a wider set of PHP extensions in the default build.
- A Cairo-owned SAPI adapted from FrankenPHP; this prototype uses PHP's embed SAPI.
