<?php

// Swoole: BENCH_THREADS worker processes, each running every request in a
// coroutine, with blocking calls hooked (SWOOLE_HOOK_ALL).

declare(strict_types=1);

use Swoole\ConnectionPool;
use Swoole\Http\Request;
use Swoole\Http\Response;
use Swoole\Http\Server;

require __DIR__ . '/../workload.php';

$workers = (int) bench_env('BENCH_THREADS', '4');
$perWorker = intdiv((int) bench_env('BENCH_DB_CONNECTIONS', '40'), $workers);

$server = new Server('0.0.0.0', 8080, SWOOLE_BASE);
$server->set([
    'worker_num' => $workers,
    'enable_coroutine' => true,
    'hook_flags' => SWOOLE_HOOK_ALL,
    'max_coroutine' => 100_000,
    'log_level' => SWOOLE_LOG_WARNING,
    'http_compression' => false,
]);

/** @var array<string, ConnectionPool> $pools */
$pools = [];
$server->on('workerStart', function () use (&$pools, $perWorker): void {
    $pools = [
        'pgsql' => new ConnectionPool(fn (): PDO => bench_pdo('pgsql'), $perWorker),
        'mysql' => new ConnectionPool(fn (): PDO => bench_pdo('mysql'), $perWorker),
        'curl' => new ConnectionPool(fn (): CurlHandle => curl_init(), 1024),
    ];
});

$with = static function (string $pool, callable $fn) use (&$pools): string {
    $conn = $pools[$pool]->get();
    try {
        return $fn($conn);
    } finally {
        $pools[$pool]->put($conn);
    }
};

$server->on('request', function (Request $req, Response $res) use ($with): void {
    [$status, $type, $body] = match ($req->server['request_uri']) {
        '/info' => [200, 'application/json', json_encode(bench_info() + ['swoole' => SWOOLE_VERSION], JSON_THROW_ON_ERROR)],
        '/hello' => [200, 'text/plain', bench_hello()],
        '/json' => [200, 'application/json', bench_json()],
        '/cpu' => [200, 'text/plain', bench_cpu()],
        '/sleep' => [200, 'text/plain', bench_sleep()],
        '/curl' => [200, 'text/plain', $with('curl', bench_curl(...))],
        '/pgsql' => [200, 'application/json', $with('pgsql', bench_query(...))],
        '/pgsql-slow' => [200, 'application/json', $with('pgsql', bench_slow_query(...))],
        '/mysql' => [200, 'application/json', $with('mysql', bench_query(...))],
        '/mysql-slow' => [200, 'application/json', $with('mysql', bench_slow_query(...))],
        default => [404, 'text/plain', 'not found'],
    };
    $res->status($status);
    $res->header('Content-Type', $type);
    $res->end($body);
});

$server->start();
