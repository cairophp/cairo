<?php

// Cairo\Sql pools are shared by every request, so a request must not leave a
// transaction or a lock on a pooled connection, and a request can bound its
// wait for a free one. Works on PostgreSQL and MySQL:
//
//     CAIRO_SQL_DSN='postgres://app:secret@127.0.0.1/app' cairo serve hardening.php
//     CAIRO_SQL_DSN='mysql://app:secret@127.0.0.1:3306/app' cairo serve hardening.php

declare(strict_types=1);

use Cairo\App;
use Cairo\Http\Request;
use Cairo\Http\Response;
use Cairo\PoolTimeout;
use Cairo\Sql\Db;
use Cairo\Sql\SqlException;
use Cairo\Sql\Tx;

use function Cairo\{async, await, sleep};

$dsn = getenv('CAIRO_SQL_DSN') ?: 'postgres://cairo:cairo@127.0.0.1:5432/cairo';
$mysql = str_starts_with($dsn, 'mysql://');

// One connection, so every request gets the connection the last one used.
$db = Db::open($dsn, ['max_open' => 1]);
// The same, but a query waits at most 0.1 s for it.
$bounded = Db::open($dsn, ['max_open' => 1, 'acquire_timeout' => 0.1]);

$app = new App();
$http = $app->http(getenv('CAIRO_HTTP') ?: ':8080');

// BEGIN on the pool is refused: the transaction would stay on the
// connection for whichever request gets it next.
$http->get('/begin', function (Request $req) use ($db): Response {
    try {
        $db->exec((string) ($req->query('sql') ?? 'BEGIN'));
        return Response::text('ran');
    } catch (SqlException $e) {
        return Response::text('refused: ' . $e->getMessage());
    }
});

// Is the pool's connection in a transaction a request left open? A fresh
// request must see no.
$http->get('/in-transaction', fn (): Response => Response::json($mysql
    ? (bool) $db->queryValue('SELECT @@in_transaction')
    : $db->queryValue('SHOW application_name') === 'left open'));

// PostgreSQL runs a multi-statement string that opens one: the pool closes
// that connection instead of handing it to the next request. MySQL refuses
// multi-statement strings.
$http->get('/sneak', function () use ($db): Response {
    try {
        $db->exec("SELECT 1; BEGIN; SET LOCAL application_name = 'left open'");
        return Response::text('ran');
    } catch (SqlException $e) {
        return Response::text('refused');
    }
});

// One task holds the only connection for 0.5 s in a transaction; the other
// gives up after 0.1 s with a PoolTimeout. Once the transaction ends, the
// connection is free again.
$http->get('/acquire-timeout', function () use ($bounded, $mysql): Response {
    $holder = async(fn () => $bounded->transaction(
        fn (Tx $tx) => $tx->queryValue($mysql ? 'SELECT SLEEP(0.5)' : 'SELECT 1 FROM pg_sleep(0.5)'),
    ));
    sleep(0.02);
    $start = microtime(true);
    try {
        $bounded->queryValue('SELECT 1');
        $error = null;
    } catch (PoolTimeout $e) {
        $error = $e->getMessage();
    }
    $took = round(microtime(true) - $start, 2);
    await($holder);
    return Response::json(['error' => $error, 'took' => $took, 'after' => (int) $bounded->queryValue('SELECT 1')]);
});

return $app;
