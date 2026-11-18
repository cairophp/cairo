<?php

// The work every server does per request, so the servers differ only in how
// they run it. Each server's entry point maps a path to one of these.

declare(strict_types=1);

const BENCH_ROWS = 1000;

function bench_env(string $name, string $default): string
{
    $value = getenv($name);
    return $value === false || $value === '' ? $default : $value;
}

function bench_hello(): string
{
    return 'Hello, World!';
}

function bench_json(): string
{
    return json_encode(['message' => 'Hello, World!', 'items' => range(1, 10)], JSON_THROW_ON_ERROR);
}

function bench_fib(int $n): int
{
    return $n < 2 ? $n : bench_fib($n - 1) + bench_fib($n - 2);
}

/** About 1-2 ms of pure PHP. */
function bench_cpu(): string
{
    return (string) bench_fib(20);
}

/** A 20 ms wait, as a slow downstream call would be. */
function bench_sleep(): string
{
    usleep(20_000);
    return 'slept';
}

function bench_upstream_url(): string
{
    return bench_env('BENCH_UPSTREAM', 'http://127.0.0.1:9100') . '/delay?ms=20';
}

/**
 * An outgoing HTTP call through curl to an upstream that answers in 20 ms.
 * Callers reuse $ch (one per worker, or a pool of them) to keep the
 * connection alive; concurrent requests must not share one.
 */
function bench_curl(CurlHandle $ch): string
{
    curl_setopt_array($ch, [
        CURLOPT_URL => bench_upstream_url(),
        CURLOPT_RETURNTRANSFER => true,
        CURLOPT_TIMEOUT => 10,
    ]);
    $body = curl_exec($ch);
    if (!is_string($body)) {
        throw new RuntimeException('upstream: ' . curl_error($ch));
    }
    return $body;
}

/** @return array{string, string, string} */
function bench_pgsql_config(): array
{
    return [
        bench_env('BENCH_PGSQL', 'pgsql:host=127.0.0.1;port=5432;dbname=cairo'),
        bench_env('BENCH_PGSQL_USER', 'cairo'),
        bench_env('BENCH_PGSQL_PASSWORD', 'cairo'),
    ];
}

/** @return array{string, string, string} */
function bench_mysql_config(): array
{
    return [
        bench_env('BENCH_MYSQL', 'mysql:host=127.0.0.1;port=3306;dbname=bench'),
        bench_env('BENCH_MYSQL_USER', 'root'),
        bench_env('BENCH_MYSQL_PASSWORD', 'cairo'),
    ];
}

/** @param array<int, mixed> $options */
function bench_pdo(string $driver, array $options = []): PDO
{
    [$dsn, $user, $password] = $driver === 'pgsql' ? bench_pgsql_config() : bench_mysql_config();
    return new PDO($dsn, $user, $password, $options + [PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION]);
}

/** One indexed row by primary key. */
function bench_query(PDO $pdo): string
{
    $st = $pdo->prepare('SELECT id, name FROM bench_items WHERE id = ?');
    $st->execute([random_int(1, BENCH_ROWS)]);
    return json_encode($st->fetch(PDO::FETCH_ASSOC), JSON_THROW_ON_ERROR);
}

/** A query that takes 10 ms on the server. */
function bench_slow_query(PDO $pdo): string
{
    $sql = $pdo->getAttribute(PDO::ATTR_DRIVER_NAME) === 'pgsql'
        ? 'SELECT 1 AS ok FROM pg_sleep(0.01)'
        : 'SELECT SLEEP(0.01) AS ok';
    return json_encode($pdo->query($sql)->fetch(PDO::FETCH_ASSOC), JSON_THROW_ON_ERROR);
}

/** @return array<string, mixed> */
function bench_info(): array
{
    $opcache = function_exists('opcache_get_status') ? opcache_get_status(false) : false;
    return [
        'php' => PHP_VERSION,
        'sapi' => PHP_SAPI,
        'zts' => (bool) PHP_ZTS,
        'opcache' => is_array($opcache) && $opcache['opcache_enabled'],
        'jit' => is_array($opcache) && ($opcache['jit']['on'] ?? false),
    ];
}
