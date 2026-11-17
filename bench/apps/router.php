<?php

// The routes for the servers that run one request per worker at a time
// (PHP-FPM, FrankenPHP, RoadRunner). A worker reuses its database connections
// and curl handle across requests.

declare(strict_types=1);

require_once __DIR__ . '/workload.php';

/** @return array{int, string, string} status, content type, body */
function bench_route(string $path): array
{
    static $pdo = [];
    static $curl = null;

    // Statics last for the worker's life in FrankenPHP and RoadRunner, but
    // only for one request in FPM, which keeps connections with ATTR_PERSISTENT.
    $db = static function (string $driver) use (&$pdo): PDO {
        return $pdo[$driver] ??= bench_pdo($driver, PHP_SAPI === 'fpm-fcgi' ? [PDO::ATTR_PERSISTENT => true] : []);
    };

    return match ($path) {
        '/info' => [200, 'application/json', json_encode(bench_info(), JSON_THROW_ON_ERROR)],
        '/hello' => [200, 'text/plain', bench_hello()],
        '/json' => [200, 'application/json', bench_json()],
        '/cpu' => [200, 'text/plain', bench_cpu()],
        '/sleep' => [200, 'text/plain', bench_sleep()],
        '/curl' => [200, 'text/plain', bench_curl($curl ??= curl_init())],
        '/pgsql' => [200, 'application/json', bench_query($db('pgsql'))],
        '/pgsql-slow' => [200, 'application/json', bench_slow_query($db('pgsql'))],
        '/mysql' => [200, 'application/json', bench_query($db('mysql'))],
        '/mysql-slow' => [200, 'application/json', bench_slow_query($db('mysql'))],
        default => [404, 'text/plain', 'not found'],
    };
}
