<?php

// Cairo: HTTP on :8080, gRPC on :9000. BENCH_THREADS PHP threads, each
// running every request in its own fiber.

declare(strict_types=1);

use Cairo\App;
use Cairo\Context;
use Cairo\Grpc\ServerStream;
use Cairo\Http\Client;
use Cairo\Http\Response;
use Cairo\Pool;
use Cairo\Sql\Db;
use Helloworld\CountRequest;
use Helloworld\GreeterInterface;
use Helloworld\HelloReply;
use Helloworld\HelloRequest;
use Helloworld\SlowHelloRequest;

require __DIR__ . '/../workload.php';
require __DIR__ . '/../../../examples/greeter/gen/autoload.php';

final class Greeter implements GreeterInterface
{
    public function SayHello(HelloRequest $request, Context $ctx): HelloReply
    {
        return new HelloReply(message: "Hello {$request->name}");
    }

    public function SlowHello(SlowHelloRequest $request, Context $ctx): HelloReply
    {
        Cairo\sleep($request->seconds);
        return new HelloReply(message: "Hello {$request->name}");
    }

    public function CountUp(CountRequest $request, ServerStream $stream, Context $ctx): void
    {
        for ($i = 1; $i <= $request->count; $i++) {
            $stream->send(new HelloReply(message: "{$request->name} $i"));
        }
    }

    public function JoinNames(ServerStream $stream, Context $ctx): HelloReply
    {
        $names = [];
        foreach ($stream as $request) {
            $names[] = $request->name;
        }
        return new HelloReply(message: 'Hello ' . implode(', ', $names));
    }

    public function Chat(ServerStream $stream, Context $ctx): void
    {
        while (($request = $stream->recv()) !== null) {
            $stream->send(new HelloReply(message: "Hello {$request->name}"));
        }
    }
}

$threads = (int) bench_env('BENCH_THREADS', '4');
$perThread = intdiv((int) bench_env('BENCH_DB_CONNECTIONS', '40'), $threads);

// Per-thread pools: a fiber borrows a connection (or curl handle) and gives
// it back when it ends.
$pg = new Pool(fn (): PDO => bench_pdo('pgsql'), max: $perThread);
$my = new Pool(fn (): PDO => bench_pdo('mysql'), max: $perThread);
$curl = new Pool(fn (): CurlHandle => curl_init(), max: 1024);

// Cairo\Sql pools live in Go and are shared by all threads.
[, $pgUser, $pgPass] = bench_pgsql_config();
[, $myUser, $myPass] = bench_mysql_config();
$sqlOptions = ['max_open' => $perThread * $threads];
$sqlPg = Db::open(bench_env('BENCH_SQL_PGSQL', "postgres://$pgUser:$pgPass@127.0.0.1:5432/cairo"), $sqlOptions);
$sqlMy = Db::open(bench_env('BENCH_SQL_MYSQL', "mysql://$myUser:$myPass@127.0.0.1:3306/bench"), $sqlOptions);
$client = new Client(['timeout' => 10]);

$app = new App();
$app->threads($threads);
$app->offloadThreads(1);

$text = static fn (string $body): Response => Response::text($body);
$json = static fn (string $body): Response => new Response(200, ['Content-Type' => 'application/json'], $body);

$http = $app->http(':8080');
$http->get('/info', fn (): Response => Response::json(bench_info() + ['server' => 'cairo']));
$http->get('/hello', fn (): Response => $text(bench_hello()));
$http->get('/json', fn (): Response => $json(bench_json()));
$http->get('/cpu', fn (): Response => $text(bench_cpu()));
// The same work behind Go's response cache: after the first request, Go
// answers alone, or serves the stale copy while one request rebuilds it.
$http->get('/cpu-cached', fn (): Response => $text(bench_cpu()))->cache(ttl: 60);
$http->get('/mysql-slow-cached', fn (): Response => $json($my->with(bench_slow_query(...))))
    ->cache(ttl: 1, staleWhileRevalidate: 10);
$http->get('/sleep', fn (): Response => $text(bench_sleep()));
$http->get('/curl', fn (): Response => $text($curl->with(bench_curl(...))));
$http->get('/client', fn (): Response => $text($client->get(bench_upstream_url())->body));
$http->get('/pgsql', fn (): Response => $json($pg->with(bench_query(...))));
$http->get('/pgsql-slow', fn (): Response => $json($pg->with(bench_slow_query(...))));
$http->get('/mysql', fn (): Response => $json($my->with(bench_query(...))));
$http->get('/mysql-slow', fn (): Response => $json($my->with(bench_slow_query(...))));
$http->get('/sql-pgsql', fn (): Response => $json(json_encode(
    $sqlPg->queryRow('SELECT id, name FROM bench_items WHERE id = $1', [random_int(1, BENCH_ROWS)]),
    JSON_THROW_ON_ERROR,
)));
$http->get('/sql-mysql', fn (): Response => $json(json_encode(
    $sqlMy->queryRow('SELECT id, name FROM bench_items WHERE id = ?', [random_int(1, BENCH_ROWS)]),
    JSON_THROW_ON_ERROR,
)));

$app->grpc(':9000')
    ->proto(__DIR__ . '/../../../examples/greeter/proto/helloworld.proto')
    ->service('helloworld.Greeter', Greeter::class);

return $app;
