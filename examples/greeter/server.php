<?php

declare(strict_types=1);

use Cairo\App;
use Cairo\Context;
use Cairo\Middleware;
use Cairo\Grpc\Code;
use Cairo\Grpc\Exception as GrpcException;
use Cairo\Grpc\ServerStream;
use Cairo\Http\Request;
use Cairo\Http\Response;
use Cairo\Pool;
use Helloworld\GreeterInterface;
use Helloworld\HelloReply;
use Helloworld\HelloRequest;
use Helloworld\CountRequest;
use Helloworld\SlowHelloRequest;

use function Cairo\all;
use function Cairo\async;
use function Cairo\await;
use function Cairo\offload;
use function Cairo\sleep;

// Message classes and GreeterInterface are generated from the .proto with
// `cairo gen --out gen proto/helloworld.proto`; no protoc needed.
require __DIR__ . '/gen/autoload.php';

final class Greeter implements GreeterInterface
{
    public function SayHello(HelloRequest $request, Context $ctx): HelloReply
    {
        if ($request->name === '') {
            throw new GrpcException(Code::INVALID_ARGUMENT, 'name is required');
        }
        return new HelloReply(message: "Hello {$request->name}");
    }

    public function SlowHello(SlowHelloRequest $request, Context $ctx): HelloReply
    {
        $remaining = $ctx->remaining();
        sleep($request->seconds);   // the fiber suspends; Go waits
        return new HelloReply(
            message: "Hello {$request->name}",
            remaining: is_finite($remaining) ? $remaining : 0.0,
        );
    }

    public function CountUp(CountRequest $request, ServerStream $stream, Context $ctx): void
    {
        for ($i = 1; $i <= $request->count; $i++) {
            if ($i > 1 && $request->interval > 0) {
                sleep($request->interval);
            }
            $stream->send(new HelloReply(message: "{$request->name} $i"));
        }
    }

    public function JoinNames(ServerStream $stream, Context $ctx): HelloReply
    {
        $names = [];
        foreach ($stream as $request) {   // HelloRequest objects, until the client is done
            $names[] = $request->name;
        }
        return new HelloReply(message: 'Hello ' . implode(', ', $names));
    }

    public function Chat(ServerStream $stream, Context $ctx): void
    {
        while (($request = $stream->recv()) !== null) {
            if ($request->name === '') {
                throw new GrpcException(Code::INVALID_ARGUMENT, 'name is required');
            }
            $stream->send(new HelloReply(message: "Hello {$request->name}"));
        }
    }
}

/** CPU-bound work: spins for $seconds. Runs on the offload pool. */
function burn(float $seconds): int
{
    $end = microtime(true) + $seconds;
    $n = 0;
    while (microtime(true) < $end) {
        $n++;
    }
    return $n;
}

$app = new App();
$app->threads('auto');          // request threads: many fibers each
$app->offloadThreads('auto');   // CPU threads for Cairo\offload()

$http = $app->http(getenv('CAIRO_HTTP') ?: ':8080')
    // Go runs these before PHP, in this order.
    ->use(Middleware::requestId())
    ->use(Middleware::cors(origins: ['*']))
    ->use(Middleware::gzip());

// The request id set by the middleware reaches PHP as a header.
$http->get('/request-id', fn (Request $req): Response => Response::text((string) $req->header('x-request-id')));

// Large enough for the gzip middleware (1 KB by default).
$http->get('/big', fn (): Response => Response::text(str_repeat("Cairo serves PHP from Go.\n", 200)));

// Multipart uploads in fiber mode: $req->form() and $req->file().
$http->post('/upload', function (Request $req): Response {
    $file = $req->file('photo');
    return Response::json([
        'caption' => $req->form()['caption'] ?? null,
        'name' => $file?->name,
        'size' => $file?->size,
        'contents' => $file?->contents(),
    ]);
});

$http->get('/hello/{name}', fn (Request $req): Response => Response::text("Hello {$req->param('name')}\n"));

// Each request sleeps without blocking the thread: one PHP thread can hold
// thousands of these at once.
$http->get('/sleep', function (Request $req): Response {
    $seconds = (float) ($req->query('seconds') ?? 1);
    sleep($seconds);
    return Response::json(['slept' => $seconds]);
});

// Two waits run concurrently inside one request: about 1s in total, not 2s.
$http->get('/parallel', function (): Response {
    $start = microtime(true);
    [$a, $b] = await(all([
        async(function () { sleep(1); return 'a'; }),
        async(function () { sleep(1); return 'b'; }),
    ]));
    return Response::json(['results' => [$a, $b], 'took' => round(microtime(true) - $start, 2)]);
});

// Runtime hooks: PHP's own usleep() suspends this fiber instead of blocking
// the thread, so existing blocking code needs no changes.
$http->get('/blocking-sleep', function (Request $req): Response {
    $seconds = (float) ($req->query('seconds') ?? 1);
    usleep((int) ($seconds * 1e6));
    return Response::json(['slept' => $seconds]);
});

// Socket hooks: a plain blocking stream client. Waiting for the reply
// suspends only this fiber; the same applies to PDO MySQL and Redis clients.
$http->get('/tcp', function (Request $req): Response {
    $addr = (string) $req->query('addr');
    $conn = @stream_socket_client(str_contains($addr, '://') ? $addr : "tcp://$addr", $errno, $errstr, 5);
    if ($conn === false) {
        return Response::json(['error' => $errstr], 502);
    }
    fwrite($conn, ($req->query('delay') ?? '1') . "\n");
    $line = fgets($conn);
    fclose($conn);
    return Response::json(['reply' => trim((string) $line)]);
});

// The same hooks make PDO MySQL (mysqlnd) fiber-aware with no code changes:
// SELECT SLEEP(1) suspends this fiber, not the thread.
$http->get('/mysql', function (Request $req): Response {
    $pdo = new PDO((string) getenv('CAIRO_TEST_MYSQL'), (string) getenv('CAIRO_TEST_MYSQL_USER'), (string) getenv('CAIRO_TEST_MYSQL_PASSWORD'));
    $row = $pdo->query('SELECT SLEEP(' . (float) ($req->query('seconds') ?? 1) . ') AS slept, 42 AS answer')->fetch(PDO::FETCH_ASSOC);
    return Response::json($row);
});

// PDO pgsql through Cairo's Go-backed driver (pgx, no libpq): pg_sleep(1)
// suspends this fiber, not the thread.
$pgsql = static fn (): PDO => new PDO((string) getenv('CAIRO_TEST_PGSQL'), (string) getenv('CAIRO_TEST_PGSQL_USER'), (string) getenv('CAIRO_TEST_PGSQL_PASSWORD'));

$http->get('/pgsql', function (Request $req) use ($pgsql): Response {
    $st = $pgsql()->prepare('SELECT 42 AS answer FROM pg_sleep(:s)');
    $st->execute(['s' => (float) ($req->query('seconds') ?? 1)]);
    return Response::json($st->fetch(PDO::FETCH_ASSOC));
});

// Everyday PDO on the same driver: placeholders, types, transactions, errors.
$http->get('/pgsql/crud', function () use ($pgsql): Response {
    $pdo = $pgsql();
    $table = 'cairo_crud_' . bin2hex(random_bytes(4));
    $pdo->exec("CREATE TEMP TABLE $table (id serial PRIMARY KEY, name text, n int, ok bool, data bytea, price numeric)");
    $st = $pdo->prepare("INSERT INTO $table (name, n, ok, data, price) VALUES (:name, :n, :ok, :data, :price)");
    $st->bindValue('name', "O'Brien");
    $st->bindValue('n', 42, PDO::PARAM_INT);
    $st->bindValue('ok', true, PDO::PARAM_BOOL);
    $st->bindValue('data', "\x00bin", PDO::PARAM_LOB);
    $st->bindValue('price', '9.99');
    $st->execute();
    $id = $pdo->lastInsertId();
    $pdo->prepare("INSERT INTO $table (name, n) VALUES (?, ?)")->execute(['second', null]);

    $pdo->beginTransaction();
    $inTx = $pdo->inTransaction();
    $pdo->exec("DELETE FROM $table");
    $pdo->rollBack();

    try {
        $pdo->query('SELECT * FROM no_such_table');
        $error = null;
    } catch (PDOException $e) {
        $error = $e->getCode();
    }
    $rows = $pdo->query("SELECT name, n, ok, encode(data, 'hex') AS data, price FROM $table ORDER BY id")->fetchAll(PDO::FETCH_ASSOC);
    return Response::json([
        'rows' => $rows,
        'id' => $id,
        'in_tx' => $inTx,
        'error' => $error,
        'quoted' => $pdo->quote("it's"),
    ]);
});

// Connect only: with Hook::CONNECT a slow connect suspends this fiber.
$http->get('/connect', function (Request $req): Response {
    $start = microtime(true);
    $conn = @stream_socket_client('tcp://' . $req->query('addr'), $errno, $errstr, (float) ($req->query('timeout') ?? 5));
    if ($conn !== false) {
        fclose($conn);
    }
    return Response::json(['connected' => $conn !== false, 'error' => $errstr, 'took' => round(microtime(true) - $start, 2)]);
});

// A pool of 2 "connections" (counting objects here; PDO in real code) shared
// by 6 concurrent tasks: they take turns, and only 2 are ever opened.
final class FakeConn
{
    public static int $opened = 0;
    public function __construct() { self::$opened++; }
}

$http->get('/pool', function (): Response {
    $pool = new Pool(fn () => new FakeConn(), max: 2);
    FakeConn::$opened = 0;
    $start = microtime(true);
    $ids = await(all(array_map(fn (int $i) => async(function () use ($pool): int {
        $conn = $pool->get();          // this task's connection until it ends
        sleep(0.2);
        return spl_object_id($conn);
    }), range(1, 6))));
    return Response::json([
        'opened' => FakeConn::$opened,
        'distinct' => count(array_unique($ids)),
        'took' => round(microtime(true) - $start, 2),
        'stats' => $pool->stats(),
    ]);
});

// curl, unchanged: while curl waits for the server, this fiber is suspended.
$http->get('/curl', function (Request $req): Response {
    $ch = curl_init((string) $req->query('url'));
    curl_setopt($ch, CURLOPT_RETURNTRANSFER, true);
    curl_setopt($ch, CURLOPT_TIMEOUT, (int) ($req->query('timeout') ?? 10));
    $body = curl_exec($ch);
    $out = [
        'body' => $body === false ? null : $body,
        'status' => curl_getinfo($ch, CURLINFO_HTTP_CODE),
        'errno' => curl_errno($ch),
        'error' => curl_error($ch),
    ];
    return Response::json($out);
});

// TLS through Go: https:// and tls:// work without ext/openssl, and the
// handshake, reads and writes suspend only this fiber.
$http->get('/https', function (Request $req): Response {
    $ssl = ['verify_peer' => $req->query('verify') !== '0'];
    if ($req->query('cafile') !== null) {
        $ssl['cafile'] = $req->query('cafile');
    }
    $body = @file_get_contents((string) $req->query('url'), false, stream_context_create(['ssl' => $ssl]));
    return Response::json(['body' => $body === false ? null : $body, 'error' => $body === false ? error_get_last()['message'] ?? '' : null]);
});

$http->get('/tls-stream', function (Request $req): Response {
    $ctx = stream_context_create(['ssl' => ['cafile' => (string) $req->query('cafile')]]);
    $fp = stream_socket_client('tls://' . $req->query('addr'), $errno, $error, 5, STREAM_CLIENT_CONNECT, $ctx);
    if ($fp === false) {
        return Response::json(['error' => $error]);
    }
    fwrite($fp, "GET /?d=0s HTTP/1.0\r\nHost: localhost\r\n\r\n");
    $raw = stream_get_contents($fp);
    fclose($fp);
    return Response::json(['response' => substr($raw, strpos($raw, "\r\n\r\n") + 4)]);
});

// A PDO constructor whose connect hangs: the fiber suspends inside it while
// PDO has warnings turned into exceptions. Other requests must not inherit that.
$http->get('/pdo-hang', function (Request $req): Response {
    try {
        new PDO('mysql:host=127.0.0.1;port=' . $req->query('port'), 'u', 'p', [PDO::ATTR_TIMEOUT => 1]);
    } catch (PDOException $e) {
        return Response::text('failed: ' . $e->getMessage());
    }
    return Response::text('connected?');
});

$http->get('/warning', function (): Response {
    try {
        $ok = @file_get_contents('/nonexistent/cairo') === false;
    } catch (Throwable $e) {
        return Response::text('threw ' . $e::class);
    }
    return Response::text($ok ? 'warning' : 'unexpected');
});

// Several transfers at once with curl_multi, the way Guzzle runs them: the
// curl_multi_select() wait suspends only this fiber.
$http->get('/curl-multi', function (Request $req): Response {
    $mh = curl_multi_init();
    $handles = [];
    foreach (explode(',', (string) $req->query('urls')) as $url) {
        $ch = curl_init($url);
        curl_setopt($ch, CURLOPT_RETURNTRANSFER, true);
        curl_multi_add_handle($mh, $ch);
        $handles[] = $ch;
    }
    do {
        $status = curl_multi_exec($mh, $active);
        if ($active && curl_multi_select($mh, 1.0) === -1) {
            usleep(1000);
        }
    } while ($active && $status === CURLM_OK);
    $bodies = [];
    foreach ($handles as $ch) {
        $bodies[] = curl_multi_getcontent($ch);
        curl_multi_remove_handle($mh, $ch);
    }
    curl_multi_close($mh);
    return Response::json(['bodies' => $bodies]);
});

// DNS through Go's resolver.
$http->get('/dns/{host}', fn (Request $req): Response => Response::json([
    'one' => gethostbyname($req->param('host')),
    'all' => gethostbynamel($req->param('host')),
]));

// Per-fiber state: child tasks see the parent's values; a child's own writes
// stay in the child; deferred callbacks run when the fiber ends.
final class Audit
{
    /** @var list<string> */
    public static array $log = [];
}

$http->get('/context/{user}', function (Request $req): Response {
    $user = $req->param('user');
    Context::set('user', $user);
    Context::defer(static function () use ($user): void { Audit::$log[] = "done $user"; });
    $child = await(async(function (): array {
        $seen = Context::get('user');
        Context::set('user', 'child');
        sleep(0.01);
        return ['seen' => $seen, 'own' => Context::get('user')];
    }));
    return Response::json([
        'user' => Context::get('user'),
        'child' => $child,
        'missing' => Context::get('nope', 'default'),
    ]);
});

$http->get('/audit', fn (): Response => Response::json(Audit::$log));

// CPU-heavy work goes to the offload pool; the request thread keeps serving
// other requests while this one waits.
$http->get('/cpu', function (Request $req): Response {
    $seconds = (float) ($req->query('seconds') ?? 1);
    return Response::json(['iterations' => offload('burn', $seconds)]);
});

// More routes, one file per feature: each returns fn (Cairo\Http\Server $http, Cairo\App $app).
foreach (glob(__DIR__ . '/routes/*.php') ?: [] as $file) {
    (require $file)($http, $app);
}

$grpc = $app->grpc(getenv('CAIRO_GRPC') ?: ':9000')
    ->proto(__DIR__ . '/proto/helloworld.proto')
    ->maxDuration(30)
    ->use(Middleware::requestId());

$grpc->service('helloworld.Greeter', Greeter::class);

return $app;
