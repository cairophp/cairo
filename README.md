# Cairo

Cairo is a PHP application server written in Go. You declare HTTP and gRPC
servers in PHP; Go does the serving. Every request runs in its own PHP Fiber,
and every fiber has its own goroutine, so one PHP thread can hold thousands of
requests at once while Go does the waiting.

> Status: milestone 1 prototype. Design doc:
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

Rules that keep fibers safe: per-request data lives on `Request` and
`Context`, not in superglobals (Cairo does not fill `$_GET`/`$_SERVER`), and
blocking calls (PDO, curl, PHP's own `sleep()`) stall every fiber on their
thread until Go-backed replacements exist. Use `Cairo\sleep()` and
`Cairo\offload()`.

## Building

Cairo links against a thread-safe (ZTS) PHP built with the embed SAPI.

```sh
make php            # builds PHP 8.3 (ZTS, embed) into /opt/php-zts
make build          # bin/cairo
make test           # unit + end-to-end tests
make run            # serves examples/greeter
```

Or with Docker: `docker build -t cairo . && docker run -p 8080:8080 -p 9000:9000 cairo`.

Requirements: Go 1.24+, a C toolchain, and PHP 8.1 or newer (fibers).

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

## What milestone 1 does not do yet

- Streaming gRPC, generated PHP message classes, a `cairo protoc` command.
- Go-backed HTTP client, SQL, Redis (only `Cairo\sleep()` and `Cairo\offload()` suspend today).
- Hard interruption of PHP code at a deadline, `isolated` mode for non-fiber-safe apps.
- Opcache and a wider set of PHP extensions in the default build.
- A Cairo-owned SAPI adapted from FrankenPHP; this prototype uses PHP's embed SAPI.
