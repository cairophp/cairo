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

`cairo gen` turns `.proto` files into PHP: a class per message (typed
properties, a named-argument constructor, `fromArray()` / `toArray()`), a class
of constants per enum, and per service an interface to implement and a typed
client. It needs no `protoc` and no `google/protobuf` runtime.

```sh
cairo gen --out gen proto/helloworld.proto     # -I DIR for imports
```

```php
require __DIR__ . '/gen/autoload.php';

final class Greeter implements Helloworld\GreeterInterface
{
    public function SayHello(HelloRequest $request, Cairo\Context $ctx): HelloReply
    {
        if ($request->name === '') {
            throw new Cairo\Grpc\Exception(Cairo\Grpc\Code::INVALID_ARGUMENT, 'name is required');
        }
        return new HelloReply(message: "Hello {$request->name}");
    }
}
```

Go decodes each call with the method's descriptor and passes it to PHP as
protojson; when a handler's first parameter is a generated class, Cairo builds
it with `fromArray()`, and a returned object goes back through `toArray()`.
Handlers that take and return plain arrays keep working.

Mapping: properties use the protojson (lowerCamelCase) field names; 64-bit
integers are PHP `int`; `bytes` are raw strings; enums are `int` with
constants; `optional` and `oneof` fields are nullable; nested types are
`Outer_Inner`; `google.protobuf.*` well-known types stay in their protojson
form (a string for `Timestamp`).

Streaming methods get a `Cairo\Grpc\ServerStream`. Server streaming handlers
take `(request, stream, ctx)` and `send()` their replies; client-streaming
and bidirectional ones take `(stream, ctx)` and `recv()` (or `foreach`) the
client's messages until `null`. A client-streaming handler returns its reply,
and an exception ends the stream with its status:

```php
public function CountUp(CountRequest $request, ServerStream $stream, Cairo\Context $ctx): void
{
    for ($i = 1; $i <= $request->count; $i++) {
        $stream->send(new HelloReply(message: "{$request->name} $i"));
    }
}

public function Chat(ServerStream $stream, Cairo\Context $ctx): void
{
    foreach ($stream as $request) {           // HelloRequest objects
        $stream->send(new HelloReply(message: "Hello {$request->name}"));
    }
}
```

Health checking (`grpc.health.v1`) and server reflection are built in, so
`grpcurl -plaintext localhost:9000 list` works.

Calling gRPC services: the generated client carries its `.proto` descriptors,
and `Cairo\Grpc\Client` takes arrays with `.proto` files. Calls run through
grpc-go on the fiber's goroutine (only the fiber waits, and the request's
deadline caps the call); connections are shared across requests and threads.

```php
$greeter = new Helloworld\GreeterClient('greeter:9000', ['timeout' => 2]);   // tls => true or [cafile, cert, key, server_name]
$reply = $greeter->SayHello(new HelloRequest(name: 'Cairo'), ['metadata' => ['x-trace' => $id]]);

$client = new Cairo\Grpc\Client('greeter:9000', ['protos' => [__DIR__ . '/proto/helloworld.proto']]);
$reply = $client->call('/helloworld.Greeter/SayHello', ['name' => 'Cairo']);       // a protojson array
$full = $client->invoke('/helloworld.Greeter/SayHello', ['name' => 'Cairo']);      // ->message, ->headers, ->trailers
```

A status other than OK throws `Cairo\Grpc\Exception` with the status code.
Streaming methods return a `Cairo\Grpc\ClientStream`:

```php
foreach ($greeter->CountUp(new CountRequest(name: 'n', count: 3)) as $reply) { ... }   // server streaming

$join = $greeter->JoinNames();                     // client streaming
$join->send(new HelloRequest(name: 'Ada'));
$reply = $join->closeAndRecv();

$chat = $greeter->Chat();                           // bidirectional
$chat->send(new HelloRequest(name: 'one'));
$reply = $chat->recv();                             // null once the server is done
$chat->closeSend();
```

`Cairo\Grpc\Client` has the same through `stream()` and `serverStream()`.
A stream keeps the request's deadline, and `close()` (or dropping the
object) cancels it.

### Middleware

Middleware is declared in `server.php` and runs in Go, before a request reaches
PHP, so a rejected or preflight request costs PHP nothing:

```php
use Cairo\Middleware;

$http->use(Middleware::requestId())                         // X-Request-Id, passed to PHP and echoed back
     ->use(Middleware::accessLog())
     ->use(Middleware::cors(origins: ['https://app.example'], credentials: true))
     ->use(Middleware::rateLimit(perSecond: 50, burst: 100)) // 429 + Retry-After, per client IP
     ->use(Middleware::maxBody(1 << 20))                    // 413 above 1 MB
     ->use(Middleware::timeout(30))                         // a deadline for every request
     ->use(Middleware::gzip());                             // responses of 1 KB or more

$grpc->use(Middleware::requestId())->use(Middleware::rateLimit(perSecond: 100));  // RESOURCE_EXHAUSTED
```

They run in the order declared; gRPC servers take `requestId()`,
`accessLog()`, `rateLimit()` and `timeout()` (the earlier of it and the
client's deadline wins).

### Context and deadlines

`Cairo\Context::current()` (also passed to handlers) exposes the call's
deadline: `remaining()`, `deadline()`, `err()`, `throwIfDone()`. A Cairo I/O
call that is still waiting when the deadline passes throws
`Cairo\Context\DeadlineExceeded`, which becomes `DEADLINE_EXCEEDED` for gRPC
and `504` for HTTP.

Code that never waits (a CPU loop, a runaway recursion) is stopped too. When
the deadline passes, Go interrupts the request's PHP thread, and at the next
loop iteration or function call the request's fiber gets
`Context\DeadlineExceeded`, so it can clean up. If the code catches it and
carries on, 50 ms later the fiber is unwound the way `exit()` does it: catch
and `finally` blocks are skipped and destructors run. Only that request
stops; other fibers on the thread carry on. The same applies to its
`async()` tasks. Cairo's own runtime code is never interrupted. A single
long call into C (one huge `preg_match()` or `sort()`) runs to its end
before the check.

### Concurrency API

```php
use function Cairo\{async, await, all, sleep, offload};

[$a, $b] = await(all([
    async(fn () => fetchUser($id)),     // each async() fiber gets its own goroutine
    async(fn () => fetchOrders($id)),
]));
```

`Cairo\Channel` passes values between fibers on the same PHP thread, as Go
channels do. `new Channel()` is unbuffered: `send()` waits for a receiver.
`new Channel(10)` buffers ten values. `select()` waits on several channels at
once:

```php
use Cairo\Channel;
use function Cairo\{async, select};

$jobs = new Channel(100);
async(function () use ($jobs) {
    foreach ($ids as $id) {
        $jobs->send($id);
    }
    $jobs->close();
});
foreach ($jobs as $id) { /* until closed and drained */ }

[$key, $value, $ok] = select([
    'job'  => $jobs,                // receive
    'out'  => [$results, $row],     // send
    'done' => $ctx->done(),         // closed when the request's context ends
], timeout: 0.5);                   // [null, null, false] after 0.5s; 0 never waits
```

A waiting fiber is suspended, not the thread. The wait also ends when the
request's context ends: with `$ctx->done()` among the cases, select() returns
that case; otherwise it throws `Context\DeadlineExceeded` or
`Context\Canceled`. After `close()`, receivers get what is buffered, then
`recv()` throws `Channel\Closed` (select() returns `ok` false), and `send()`
throws `Channel\Closed`. `trySend()` and `tryRecv()` never wait. Values stay
in PHP and are not copied, so a channel cannot cross PHP threads; use
`offload()` for that.

### Runtime hooks

Cairo swaps the handlers of some blocking PHP functions so that, inside a
request fiber, they suspend the fiber while its goroutine waits, and existing
code needs no changes. Outside request fibers (the declare phase, offload
threads, fibers your own code creates) the original functions run.

| Hook | Functions | Notes |
|---|---|---|
| `Hook::SLEEP` | `sleep()`, `usleep()`, `time_nanosleep()` | |
| `Hook::SOCKETS` | reads and writes on `tcp://` and `unix://` streams: mysqlnd (PDO MySQL, mysqli), Predis, phpredis, `fsockopen()`, `stream_socket_client()` | waits go through one Go epoll loop |
| `Hook::CONNECT` | connects on those streams, DNS included, and TLS handshakes | Go's dialer; falls back to PHP when a `bindto` option is set |
| `Hook::DNS` | `gethostbyname()`, `gethostbynamel()` | Go's resolver |
| `Hook::CURL` | `curl_exec()`, `curl_multi_select()` (Guzzle's multi handler) | libcurl still does the transfer; only its socket waits move to Go, so `curl_getinfo()`, `curl_error()` and callbacks are unchanged |
| `Hook::PGSQL` | PDO `pgsql:` queries | Cairo's own driver; see PostgreSQL below |

```php
$app->hooks(Hook::ALL);    // the default; Hook::NONE turns them off
```

A hooked call honours the request's deadline like `Cairo\sleep()` does.
Socket waits go through one Go epoll loop, so a thousand waiting queries do not
hold a thousand OS threads. With one PHP thread, 50 PDO queries running
`SELECT SLEEP(1)` finish in about 1 second (20 take 20 seconds with
`Hook::NONE`).

### PostgreSQL

Cairo has its own PDO driver for `pgsql:` DSNs, written in C on the PDO side
and backed by Go's pgx (`pgconn`) on the other, so it needs no libpq. Inside a
request fiber each query runs on the fiber's goroutine (`Hook::PGSQL`): the
fiber suspends, and the request's deadline cancels the query. With one PHP
thread, 50 requests running `pg_sleep(1)` finish in about 1 second (50
seconds with the hook off).

```php
$pdo = new PDO('pgsql:host=127.0.0.1;port=5432;dbname=app', 'app', 'secret');
$st = $pdo->prepare('SELECT * FROM users WHERE id = :id');
$st->execute(['id' => 7]);
```

Prepared statements, `?` and `:name` placeholders, transactions,
`lastInsertId()`, `quote()`, `getColumnMeta()` and the error modes work as
with pdo_pgsql; `int2/int4/int8` and `bool` columns come back as PHP ints and
bools, and `bytea` as a read-only stream, as with pdo_pgsql. Results are read in
full, like libpq's `PQexec`. Use one `PDO` per fiber (for example through
`Cairo\Pool`): a connection runs one query at a time. The driver registers
only when PHP has no pdo_pgsql of its own; the `PGSQL_ATTR_*` attributes
are not provided.

LISTEN/NOTIFY, COPY and large objects work through the same methods as pdo_pgsql:

```php
$pdo->exec('LISTEN jobs');
// Waits up to 5 s; in a request fiber only this fiber waits.
$note = $pdo->pgsqlGetNotify(PDO::FETCH_ASSOC, 5000); // ['message' => 'jobs', 'pid' => ..., 'payload' => ...] or false
$pid = $pdo->pgsqlGetPid();

$pdo->pgsqlCopyFromArray('items', ["1\tfirst", "2\tsecond"]);
$pdo->pgsqlCopyFromFile('items', '/tmp/items.tsv', "\t", '\\N', 'id, name');
$rows = $pdo->pgsqlCopyToArray('items');      // one line per row, with "\n"
$pdo->pgsqlCopyToFile('items', '/tmp/out.tsv');

$pdo->beginTransaction();                     // large objects need a transaction
$oid = $pdo->pgsqlLOBCreate();                // the new object's OID, as a string
$lob = $pdo->pgsqlLOBOpen($oid, 'w');         // a PHP stream: fwrite, fread, fseek, ftell
fwrite($lob, $bytes);
fclose($lob);
$pdo->commit();
$pdo->pgsqlLOBUnlink($oid);
```

Notifications that arrive while the connection runs other queries are kept
until `pgsqlGetNotify()` takes them. COPY data is held in memory in one
piece, both ways. Each large object read, write or seek is one statement on
the server's `lo_*` functions, run by Go, so in a fiber only the fiber waits.

### TLS

Cairo provides the `ssl://`, `tls://` and `tlsv1.x://` transports itself, and
an `https://` wrapper when PHP has none. Go's `crypto/tls` does the handshake
and encryption, and PHP gets a plain socket to Go, so `file_get_contents('https://…')`,
`stream_socket_client('tls://…')` and anything built on them work without
ext/openssl and suspend only the fiber. The `ssl` context options
`verify_peer`, `verify_peer_name`, `peer_name`, `SNI_server_name`,
`allow_self_signed`, `cafile`, `capath`, and client certificates through
`local_cert`, `local_pk` and `passphrase` are honoured.
`stream_socket_enable_crypto()` upgrades an open `tcp://` connection
(STARTTLS, as SMTP and IMAP clients do): Go handshakes on the fiber's
goroutine and the stream carries on over TLS. Only the client side is
offered. `stream_socket_enable_crypto($s, false)` turns TLS off again, as FTP
clients do after `CCC`: Go sends what PHP wrote, then a close_notify, waits
for the peer's (up to the stream's timeout), and the stream carries on in
clear text; it can turn TLS on again later. When PHP is built with
ext/openssl, its transports still serve code outside Cairo's fibers.

### HTTP client

`Cairo\Http\Client` sends requests through Go's `net/http`: pooled
keep-alive connections, HTTP/2 over TLS, and proxies from `HTTP_PROXY`,
`HTTPS_PROXY` and `NO_PROXY`. In a handler or `async()` task the fiber
suspends while Go waits, and the request's deadline cancels the call;
elsewhere (the declare phase, offload threads) the thread waits.

```php
use Cairo\Http\Client;

$api = new Client(['base_uri' => 'https://api.example.com', 'timeout' => 5, 'headers' => ['Authorization' => "Bearer $token"]]);

$user = $api->get('/users/42', ['query' => ['expand' => 'teams']])->json();
$res = $api->post('/users', ['json' => ['name' => 'Mohamed']]);
$res->status;               // 201
$res->header('Location');   // any case
$res->body;                 // string

[$a, $b] = Cairo\await(Cairo\all([
    Cairo\async(fn () => $api->get('/a')),
    Cairo\async(fn () => $api->get('/b')),
]));
```

Options, on the constructor or per request: `base_uri`, `headers`, `query`,
`body` / `json` / `form`, `timeout` (seconds, default 30), `follow_redirects`
(bool or a count, default 5), `verify` (bool or a CA bundle path) and `cert`
(a PEM path, or `[cert, key]`). Every status comes back as a response;
`Cairo\Http\ClientException` (with `->kind`: `connect`, `tls`, `timeout`,
`redirects`, `request`) means there was none. Bodies are read in full.

### Cairo\Net

Sockets and DNS from Go's `net` package, for protocols without a PHP client.
In a fiber only the fiber waits, and the request's deadline interrupts a
blocked read, write or accept.

```php
use Cairo\Net;

$conn = Net\dial('tcp', 'redis:6379', ['timeout' => 2]);   // also tls, unix, udp
$conn->setTimeout(5)->write("PING\r\n");
$conn->readLine("\r\n");          // "+PONG\r\n"; also read(), readExactly()

$server = Net\listen('tcp', '127.0.0.1:0');               // or 'tls' with cert/key
Cairo\async(function () use ($server) {
    $peer = $server->accept();
    while (($line = $peer->readLine()) !== null) {
        $peer->write($line);
    }
});

$udp = Net\listenPacket('udp', '0.0.0.0:0');
$udp->writeTo('ping', '10.0.0.5:9999');
[$data, $from] = $udp->readFrom(timeout: 1);

$dns = new Net\Resolver('1.1.1.1:53');                     // or the system resolver
$dns->lookupMX('example.com');   // also lookupHost/IPv4/IPv6/Addr/CNAME/TXT/NS/SRV
```

`tls` dials take the same options as PHP's `ssl` stream context. Failures
throw `Cairo\Net\NetException` with a `kind` (`dial`, `tls`, `timeout`,
`closed`, `eof`, `not_found`, ...); `TimeoutException` for timeouts.

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
blocking calls that are not hooked yet stall every fiber on their thread. Use `Cairo\offload()` for CPU-heavy work.

### Isolated mode

Apps that keep request state in globals or singletons (many Laravel and
Symfony apps, most legacy code) can run with one request per PHP thread at a
time:

```php
$app->concurrency('isolated')
    ->resetAfterRequest($container->get(CurrentUser::class), fn () => Cache::flushLocal());

$http->get('/legacy', function (): void {     // classic PHP works
    header('Content-Type: application/json');
    echo json_encode(['id' => $_GET['id'], 'ip' => $_SERVER['REMOTE_ADDR']]);
});
```

In isolated mode Cairo fills `$_GET`, `$_POST` (url-encoded and multipart
forms), `$_FILES`, `$_COOKIE`, `$_SERVER` and `$_REQUEST`; a handler that returns nothing answers with what it
echoed, its `header()` calls and `http_response_code()`. After each request it
clears the superglobals and calls `reset()` on everything given to
`resetAfterRequest()` (any object with a `reset()` method, such as
`Cairo\Resettable` or Symfony's `ResetInterface`, or a callable). Requests that
arrive while a thread is busy wait in line for it, so use more threads in
this mode.

Uploads are parsed by Go into temporary files that are removed after the
request. `move_uploaded_file()` and `is_uploaded_file()` accept them in both
modes; in fiber mode use `$req->form()` and `$req->file('photo')`, which gives a
`Cairo\Http\UploadedFile` with `name`, `type`, `size`, `path`, `contents()` and
`moveTo()`.

### Thread supervision

A fatal error (such as running out of `memory_limit`) ends the whole PHP
thread, not only the request that caused it. Cairo answers the requests that
thread was running with 500 (gRPC `INTERNAL`), logs the PHP error, and starts
a fresh thread in its place; requests still waiting for that thread are served
by the new one. A thread that keeps dying right after it starts stops the
server instead of restarting forever.

To keep slow leaks in check, a request thread can be replaced on purpose:

```php
$app->maxRequests(10_000)   // after this many requests
    ->maxMemory('256M');    // or once memory_get_usage() reaches this, checked after each request
```

Cairo starts the replacement first and sends new requests to it, then lets the
old thread finish the requests it holds before it exits, so no request fails.
`examples/supervise` shows both.

## Building

Cairo links against a thread-safe (ZTS) libphp, built with `--enable-embed`
for the library, and runs PHP as its own SAPI: `PHP_SAPI` is `cairo`.

```sh
make php            # builds PHP 8.3 (ZTS, embed) into /opt/php-zts
make build          # bin/cairo
make test           # unit + end-to-end tests (set CAIRO_TEST_MYSQL to include PDO MySQL)
make run            # serves examples/greeter
```

Or with Docker: `docker build -t cairo . && docker run -p 8080:8080 -p 9000:9000 cairo`.

`make php` builds PHP with opcache and the extensions most applications
expect: mbstring, intl, openssl, sodium, zip, gd (JPEG, WebP, FreeType), bcmath,
gmp, calendar, exif, sockets, iconv, zlib, bz2, curl, PDO with MySQL and SQLite,
mysqli, sqlite3, and PHP's defaults (ctype, dom, fileinfo, filter, json,
session, simplexml, tokenizer, xml and the rest). `scripts/build-php.sh` lists
the system packages they need.

Opcache is loaded as a Zend extension at startup, so scripts are compiled once
per process and shared by all PHP threads. It does not accept the cairo SAPI by
name, so Cairo presents itself under another name only while opcache starts;
`PHP_SAPI` is still `cairo`. Its usual ini settings apply, and `CAIRO_OPCACHE=0`
leaves it out.

PHP settings come from a php.ini: `cairo serve --php-ini FILE` (or
`CAIRO_PHP_INI`, or `PHPRC`), otherwise PHP's own search, never the current
directory. Without one, errors are logged and not displayed. PHP's log lines
(errors, warnings, `error_log()`) go to Cairo's logger with `source=php`,
unless `error_log` names a file. `max_execution_time` and `max_input_time`
stay off whatever the ini says, because each PHP thread runs one long request;
request deadlines come from `Middleware::timeout()` and gRPC deadlines. `dl()`
is not available.

Requirements: Go 1.24+, a C toolchain, the -dev packages listed in `scripts/build-php.sh`, and PHP 8.1 or newer (fibers).

## Layout

```
cmd/cairo/            the binary (serve, gen)
internal/codegen/     cairo gen: PHP classes and clients from .proto files
internal/sapi/        embedded PHP: C bridge, PHP threads, fiber goroutines, offload pool
internal/spec/        the server declaration sent by server.php
internal/httpserver/  net/http server built from the declaration
internal/middleware/  Go middleware declared from PHP (HTTP and gRPC)
internal/grpcserver/  grpc-go server, proto compilation, health, reflection
php/runtime.php       the PHP side: Cairo\ classes and the fiber scheduler (embedded in the binary)
examples/greeter/     HTTP + gRPC example
examples/isolated/    isolated mode: superglobals, echo/header(), resets
examples/supervise/   thread restart after a fatal error, maxRequests and maxMemory
test/                 end-to-end tests
```
