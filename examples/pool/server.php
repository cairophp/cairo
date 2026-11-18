<?php

// Cairo\Pool: one connection per fiber, a bounded wait when all are in use,
// a reset before a connection is reused, and lifetimes that retire old ones.

declare(strict_types=1);

use Cairo\App;
use Cairo\Http\{Request, Response};
use Cairo\Pool;
use Cairo\PoolTimeout;

use function Cairo\{async, await, sleep};

/** Stands in for a database connection, and counts how many were opened. */
final class Conn
{
    public static int $opened = 0;
    /** Requests that got the shared connection from /quick. */
    public static int $served = 0;
    public bool $dirty = false;
    public function __construct() { self::$opened++; }
}

$app = new App();
$http = $app->http(getenv('CAIRO_HTTP') ?: ':8080');

// One connection for the whole thread, so a second request has to wait. With
// an acquireTimeout, a request whose client goes away stops waiting at once.
$one = new Pool(fn () => new Conn(), max: 1, acquireTimeout: 5);

$http->get('/hold', function (Request $req) use ($one): Response {
    $one->get();
    sleep((float) ($req->query('s') ?? 0.5));
    return Response::text('held');
});
$http->get('/quick', function () use ($one): Response {
    $conn = $one->get();    // throws when the client goes away while it waits
    Conn::$served++;
    return Response::text('conn ' . spl_object_id($conn));
});
$http->get('/stats', fn (): Response => Response::json($one->stats() + ['opened' => Conn::$opened, 'served' => Conn::$served]));

// acquireTimeout: the second task gives up after 0.1 s instead of waiting
// for the first one's 0.5 s.
$http->get('/timeout', function (): Response {
    $pool = new Pool(fn () => new Conn(), max: 1, acquireTimeout: 0.1);
    $holder = async(function () use ($pool): void {
        $pool->get();
        sleep(0.5);
    });
    sleep(0.01);
    $start = microtime(true);
    try {
        $pool->get();
        $error = null;
    } catch (PoolTimeout $e) {
        $error = $e->getMessage();
    }
    $took = round(microtime(true) - $start, 2);
    await($holder);
    return Response::json(['error' => $error, 'took' => $took, 'stats' => $pool->stats()]);
});

// The default reset: a transaction a request left open is rolled back
// before the next fiber gets the connection.
$http->get('/reset', function (): Response {
    $pool = new Pool(fn () => new PDO('sqlite::memory:'), max: 1);
    $pool->with(function (PDO $pdo): void {
        $pdo->exec('CREATE TABLE t (n INTEGER)');
        $pdo->beginTransaction();
        $pdo->exec('INSERT INTO t VALUES (1)');   // and no commit
    });
    return Response::json($pool->with(fn (PDO $pdo): array => [
        'in_transaction' => $pdo->inTransaction(),
        'rows' => (int) $pdo->query('SELECT COUNT(*) FROM t')->fetchColumn(),
    ]));
});

// The same on PostgreSQL, where the transaction was opened with plain SQL
// and then failed: the pool still sees it and rolls it back.
$http->get('/reset-pgsql', function (): Response {
    $pool = new Pool(fn () => new PDO(
        (string) getenv('CAIRO_TEST_PGSQL'),
        (string) getenv('CAIRO_TEST_PGSQL_USER'),
        (string) getenv('CAIRO_TEST_PGSQL_PASSWORD'),
        [PDO::ATTR_ERRMODE => PDO::ERRMODE_SILENT],
    ), max: 1);
    $states = $pool->with(function (PDO $pdo): array {
        $states = ['start' => $pdo->inTransaction()];
        $pdo->exec('BEGIN');
        $states['open'] = $pdo->inTransaction();
        $pdo->exec('SELECT 1/0');   // the transaction is now failed
        $states['failed'] = $pdo->inTransaction();
        return $states;
    });
    return Response::json($states + $pool->with(fn (PDO $pdo): array => [
        'next' => $pdo->inTransaction(),
        'works' => $pdo->query('SELECT 1')->fetchColumn() == 1,
    ]));
});

// A custom reset runs on every return; a connection whose reset throws is
// dropped and the next fiber gets a new one.
$http->get('/reset-custom', function (): Response {
    Conn::$opened = 0;
    $resets = 0;
    $pool = new Pool(fn () => new Conn(), max: 1, reset: function (Conn $c) use (&$resets): void {
        $resets++;
        if ($c->dirty) {
            throw new RuntimeException('connection is dirty');
        }
    });
    $first = $pool->with(fn (Conn $c): int => spl_object_id($c));
    $again = $pool->with(function (Conn $c): int {
        $c->dirty = true;
        return spl_object_id($c);
    });
    $pool->with(fn (Conn $c): bool => true);
    return Response::json(['reused' => $first === $again, 'resets' => $resets, 'opened' => Conn::$opened, 'stats' => $pool->stats()]);
});

// maxLifetime and maxIdleTime: a connection past either is not handed out.
$http->get('/lifetime', function (Request $req): Response {
    Conn::$opened = 0;
    $pool = $req->query('by') === 'idle'
        ? new Pool(fn () => new Conn(), max: 2, maxIdleTime: 0.1)
        : new Pool(fn () => new Conn(), max: 2, maxLifetime: 0.1);
    $pool->with(fn (Conn $c): bool => true);
    $pool->with(fn (Conn $c): bool => true);   // reused: still young
    $before = Conn::$opened;
    sleep(0.2);
    $pool->with(fn (Conn $c): bool => true);   // too old now: a new one
    return Response::json(['before' => $before, 'after' => Conn::$opened, 'stats' => $pool->stats()]);
});

// PostgreSQL with cairo_pool=2 in the DSN: PDO objects on every thread share
// 2 server connections. Each borrows one for the request that uses it.
$pgShared = new Pool(fn () => new PDO(
    getenv('CAIRO_TEST_PGSQL') . ';cairo_pool=2',
    (string) getenv('CAIRO_TEST_PGSQL_USER'),
    (string) getenv('CAIRO_TEST_PGSQL_PASSWORD'),
    [PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION],
), max: 8);

$http->get('/pgsql-shared', function (Request $req) use ($pgShared): Response {
    $pdo = $pgShared->get();
    $pid = (int) $pdo->query('SELECT pg_backend_pid()')->fetchColumn();
    $pdo->prepare('SELECT pg_sleep(?)')->execute([(float) ($req->query('s') ?? 0)]);
    if ($req->query('open') !== null) {
        $pdo->beginTransaction();   // left open: rolled back when the request ends
        $pdo->exec('SET LOCAL application_name = \'left open\'');
    }
    return Response::json([
        'pid' => $pid,
        'app' => $pdo->query('SHOW application_name')->fetchColumn(),
    ]);
});

return $app;
