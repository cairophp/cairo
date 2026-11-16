# Cairo

Cairo is a PHP application server written in Go. You declare HTTP and gRPC
servers in PHP; Go does the serving. Every request runs in its own PHP Fiber,
and every fiber has its own goroutine, so one PHP thread can hold thousands of
requests at once while Go does the waiting.

> Status: v0.1.0, the first release. The PHP API may still change before 1.0;
> [CHANGELOG.md](CHANGELOG.md) lists what changed. Design doc:
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

## Install

Cairo ships as one static binary for Linux on amd64 and arm64, with PHP 8.5,
opcache and the extensions built in. Nothing else is needed at run time, not even
libc, so it also runs in `FROM scratch` images. Download it from the
[releases page](https://github.com/mohamed-samir907/cairo/releases) and check
it against `SHA256SUMS`:

```sh
curl -fsSLO https://github.com/mohamed-samir907/cairo/releases/download/v0.1.0/cairo-linux-amd64
curl -fsSLO https://github.com/mohamed-samir907/cairo/releases/download/v0.1.0/SHA256SUMS
sha256sum --check --ignore-missing SHA256SUMS
install -m 755 cairo-linux-amd64 /usr/local/bin/cairo
cairo version
```

While the repository is private, download with `gh release download v0.1.0`
instead. In a container:

```dockerfile
FROM scratch
COPY cairo-linux-amd64 /cairo
COPY . /app
WORKDIR /app
ENTRYPOINT ["/cairo", "serve", "server.php"]
```

To build it yourself, see [Building](#building). For your IDE and PHPStan, add
the stubs package ([IDE and static analysis](#ide-and-static-analysis)).

### Supported platforms

- Linux on amd64 and arm64. macOS and Windows are not supported, since Cairo
  relies on a thread-safe PHP embedded in a Go process; use the binary in
  Docker there.
- PHP 8.1 or newer when you build Cairo yourself; the release binary has
  PHP 8.5.
- PostgreSQL (through pgx, for `pdo_pgsql` and `Cairo\Sql`) and MySQL or
  MariaDB (PDO MySQL and mysqli through hooked streams, and `Cairo\Sql`).

### Known limitations

- **Fiber safety.** Requests on a thread share globals and static properties.
  In the default fiber mode, keep request state on `Request` and `Context`;
  Cairo does not fill `$_GET`, `$_POST` or `$_SERVER`. Run code that relies on
  them, or on per-request singletons, with `->concurrency('isolated')`.
- **Blocking extensions.** Only what the runtime hooks cover suspends a
  fiber: sleep, PHP streams and sockets, DNS, curl, and `pgsql:`. Other C
  extensions that open their own sockets (the MongoDB driver, gRPC's C
  extension, libmysqlclient builds) block the whole thread;
  `App::detectBlocking()` finds them.
- **Extensions.** The release binary has a fixed set of extensions
  ([Building](#building)) and cannot load others (`dl()` is off). phpredis,
  imagick, mongodb and other PECL extensions are not included; build Cairo
  yourself to add them, as long as they are thread-safe under ZTS.
- **One process.** Cairo runs one process with many PHP threads; it does not
  fork. A fatal error in PHP kills only its thread, which is replaced, but a
  crash in C code takes the process down: run it under a supervisor
  (systemd, Kubernetes) that restarts it.
- **Not included:** HTTP/3, automatic TLS certificates, and a file watcher
  for development reloads.

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
   `[Class::class, 'staticMethod']`, with serializable arguments, or a
   `Cairo\OffloadTask` object: it is serialized with its input, and its
   `run()` runs on an offload thread (`offload(new Thumbnail($path, 200))`).
   A job that throws raises `Cairo\OffloadException` naming the original
   exception class.

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

A part of a request can get its own, shorter budget, as with Go's
`context.WithTimeout`:

```php
$profile = $ctx->withTimeout(0.3)->run(function () use ($client) {
    return $client->get('http://profiles/me');   // gives up after 0.3s
});

$c = $ctx->withCancel();
$task = $c->run(fn () => async(fn () => crawl($urls)));
// ...
$c->cancel();   // the crawl's waiting calls throw Context\Canceled
```

`withTimeout()`, `withDeadline()` and `withCancel()` return a child context.
It ends at its own deadline, on `cancel()`, or when the context it came from
ends. `run($fn)` makes it the current context while `$fn` runs. Every Cairo
call waiting there (HTTP, gRPC, sockets, SQL, `sleep()`, channels) then
throws when it ends, and `async()` tasks started there inherit it. The
request goes on afterwards. Only the request's own deadline stops code that
never waits. `$ctx->done()` is a channel that closes when the context ends,
for `select()`. `$ctx->value('request_id')` reads values that Go middleware
attached to the request. Values your own code stores use `Context::set()` and
`Context::get()`.

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

### Cairo\Sql

A small typed API over Go's `database/sql`, for PostgreSQL (pgx) and MySQL or
MariaDB (go-sql-driver/mysql). The pool
lives in Go and is shared by every PHP thread: `Db::open()` with the same DSN
and options returns the same pool on each thread, so `max_open` bounds the
connections of the whole server rather than of each thread.

```php
use Cairo\Sql\Db;

$db = Db::open('postgres://app:secret@127.0.0.1/app', ['max_open' => 20]);

$users = $db->query('SELECT id, name FROM users WHERE team = $1', [$team]);   // list of rows
$user  = $db->queryRow('SELECT * FROM users WHERE id = $1', [$id]);           // a row or null
$count = $db->queryValue('SELECT count(*) FROM users');
$n     = $db->exec('UPDATE users SET seen = now() WHERE id = $1', [$id]);    // rows changed

$db->transaction(function (Cairo\Sql\Tx $tx) use ($from, $to, $amount): void {
    $tx->exec('UPDATE accounts SET balance = balance - $1 WHERE id = $2', [$amount, $from]);
    $tx->exec('UPDATE accounts SET balance = balance + $1 WHERE id = $2', [$amount, $to]);
});   // committed, or rolled back if the callback throws
```

Queries suspend only the calling fiber, and the request's deadline cancels
them. Integers, floats, booleans, text and nulls come back as PHP values;
bytea as a string, timestamps as RFC 3339 strings in UTC, and numeric, json and the
rest in PostgreSQL's text form. Pass binary parameters as
`new Cairo\Sql\Bytes($raw)`. Failures throw `Cairo\Sql\SqlException` with
`$sqlState`. A transaction left open when its fiber ends is rolled back.
`examples/sql` has more. PDO with `pgsql:` keeps working beside it.

For MySQL, open a `mysql://` URL and use `?` placeholders:

```php
$db = Db::open('mysql://app:secret@127.0.0.1:3306/app');   // ?socket=/run/mysqld/mysqld.sock for a Unix socket
$user = $db->queryRow('SELECT * FROM users WHERE id = ?', [$id]);
```

Text columns come back as strings and binary ones (BLOB, VARBINARY) as
strings too, DATETIME and TIMESTAMP as RFC 3339 in UTC, DATE as `Y-m-d`, and
DECIMAL as text. `SqlException::$sqlState` is MySQL's SQLSTATE, and
`$detail` names the MySQL error number. Other go-sql-driver/mysql parameters
(`tls`, `timeout`, `loc`, ...) go in the URL's query. `examples/sql/mysql.php`
is the same example as `server.php` for MySQL. PDO MySQL keeps working too,
through the hooked streams.

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

### Frameworks: PSR-15, Symfony and Laravel

Route handlers and `$http->fallback()` also accept framework objects:

```php
$http->fallback($psr15Handler);   // Psr\Http\Server\RequestHandlerInterface (Slim, Mezzio, ...)
$http->fallback($symfonyKernel);  // Symfony\Component\HttpKernel\HttpKernelInterface
$http->fallback($laravel->make(Illuminate\Contracts\Http\Kernel::class));
$http->post('/webhooks/{source}', new WebhookHandler());   // PSR-15 on one route
```

`fallback()` takes every request that no route or static path matches, with
any method. PSR-15 handlers get a PSR-7 request built with nyholm/psr7 or
guzzlehttp/psr7, whichever is installed. It carries headers, query, cookies,
the parsed form, uploaded files, and route parameters as attributes. Symfony
and Laravel kernels get an HttpFoundation request, and `terminate()` runs
after the response is sent. Streamed responses work too. The request's
`Cairo\Context` is the `cairo.context` attribute. `Cairo\Http\Adapter` does the
conversions by hand: `psr15()`, `symfony()`, `toPsr7()`, `fromPsr7()`,
`toSymfony()` and `fromSymfony()`.

Most framework apps keep request state in services and globals, so run them
with `->concurrency('isolated')` until they are known to be fiber-safe. See
`examples/frameworks`.

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

`$app->maxInFlight(100)` caps the requests each request thread holds at once.
Requests over the cap wait in Go until one finishes, or until their own
deadline. A waiting request costs a goroutine, not PHP memory.
`examples/supervise` shows both.

### Finding blocking code

Fibers share their PHP thread, so a request that runs a long CPU loop or a
blocking C call Cairo does not hook stalls every other request on that thread.
In development, turn on the detector:

```php
$app->detectBlocking(0.1);   // seconds a request may hold its thread without yielding
```

Go watches each request thread. When one stays in PHP past the threshold, the
running fiber logs once, at its next safe point, how long it held the thread
and where it was:

```
cairo: blocking: request 5 kept its PHP thread busy for 109ms; other requests on the thread waited. At:
  /app/routes/report.php:30 in {closure}()
```

Calls Cairo hooks (`sleep()`, `usleep()`, curl, sockets, PDO pgsql and so on)
yield, so they are never reported. The check costs a timer tick in Go and
nothing in PHP until it fires. Isolated mode, which runs one request per thread,
does not report. `examples/blocking` has a loop, a long `password_hash()` and
two calls that yield.

### Amp and Revolt

Libraries built on [Revolt](https://revolt.run) (Amp v3, and ReactPHP through
its fiber adapters) run on Cairo's scheduler instead of a second event loop:

```php
require __DIR__ . '/vendor/autoload.php';

Cairo\Revolt\Driver::install();   // once, in server.php
```

`Amp\delay()`, `Amp\async()`, futures, cancellations and Amp's sockets and
HTTP client then wait in Go like any other Cairo call, so one PHP thread
serves many such requests at once. In the example, 20 `amphp/http-client`
requests to a 1s endpoint take about 1s on one thread. Each Revolt callback
runs in an `async()` task: `defer()` and `queue()` as soon as the current
fiber yields, timers on Go timers, and `onReadable()`/`onWritable()` when Go's
poller sees the socket ready. A `Suspension` parks its fiber in Go.

Some things differ from Revolt's own drivers:

- A callback belongs to the request that scheduled it, and ends with it.
- `queue()` and `defer()` run in the order they were scheduled.
- `onSignal()` throws `UnsupportedFeatureException`, because Go handles signals.
- An exception from a callback with no error handler is logged, not thrown out of `run()`.

See `examples/revolt`.

### IDE and static analysis

The `Cairo\` classes are built into the binary, so an IDE cannot find them on
its own. `stubs/cairo.php` has the whole public API (signatures and docblocks,
no bodies) and ships as a Composer package. Until it is on Packagist, add the
repository and require it as a dev dependency:

```json
{
    "repositories": [{"type": "vcs", "url": "https://github.com/mohamed-samir907/cairo"}],
    "require-dev": {"mohamed-samir907/cairo": "dev-main"}
}
```

PhpStorm and Intelephense index it from `vendor/`. For PHPStan, include
`vendor/mohamed-samir907/cairo/stubs/phpstan.neon` (phpstan/extension-installer
does that for you). For Psalm, list `stubs/cairo.php` under `<stubs>`. The
package has no autoload entry on purpose: never `require` the stubs, since
Cairo already declares these classes.

`make stubs` regenerates the file from `php/runtime.php`, and a test fails
when it is stale.

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

### One static binary

```sh
make static         # docker build -f Dockerfile.static --output dist .
./dist/cairo serve server.php
```

`dist/cairo` holds Cairo, PHP and every extension above, linked against musl
with no shared libraries, so it runs on any Linux of the same CPU
architecture, including `FROM scratch` images. It uses PHP 8.5 with opcache
built in; pass `--build-arg PHP_VERSION=8.4.14` for another PHP. Before PHP 8.5
it has no opcache, since older PHP can only build opcache as a shared
extension; code is then compiled once per thread, when `server.php` and the
classes it loads are first used. CI builds it and runs the end-to-end tests
against it.
`scripts/build-static.sh` does the work and also runs outside Docker on Alpine.
The first build takes about 15 minutes, nearly all of it ICU and PHP. Docker
keeps those in their own layer, so later builds only compile Cairo, until
`PHP_VERSION` or the script changes. Outside Docker, set `WORK` to a fixed
directory for the same effect.

## License

Cairo is licensed under the [Apache License 2.0](LICENSE).

Cairo's SAPI is adapted from PHP's embed SAPI (PHP License 3.01) and follows
FrankenPHP (MIT) in places; `third_party/NOTICES` has both licenses. The static
binary also links PHP, its extensions and their libraries, under their own
licenses.

## Contributing

[AGENTS.md](AGENTS.md) describes how to build, test and change Cairo, for
people and coding agents alike. Changes go through pull requests against
`main`; add a line to `CHANGELOG.md` under Unreleased.

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
php/revolt.php        Cairo\Revolt\Driver, loaded when revolt/event-loop is installed
examples/greeter/     HTTP + gRPC example
examples/isolated/    isolated mode: superglobals, echo/header(), resets
examples/frameworks/  PSR-15 handlers and Symfony-style kernels (composer install first)
examples/supervise/   thread restart after a fatal error, maxRequests and maxMemory
examples/blocking/    detectBlocking: logs requests that hold their PHP thread

examples/revolt/      Amp and Revolt on Cairo's scheduler (Composer project)
examples/sql/         Cairo\Sql: PostgreSQL (server.php) and MySQL (mysql.php) through Go's database/sql

stubs/                Cairo's PHP API as stubs for IDEs (generated by scripts/stubs.php)
test/                 end-to-end tests
third_party/NOTICES   licenses of code Cairo adapts (PHP embed SAPI, FrankenPHP)
AGENTS.md             how to build, test and change Cairo (CLAUDE.md points here)
CHANGELOG.md          what changed in each release
LICENSE, NOTICE       Apache-2.0
```
