<?php

// The repeated query detector: a request that runs the same statement 10
// times or more is logged with its route, the count and the statement's
// shape, with no code in the handlers.
//
//     cairo serve server.php
//     curl localhost:8080/orders
//
//     level=WARN msg="repeated query" route="GET /orders" count=12 query="SELECT ? AS order_id"
//
// CAIRO_PDO_DSN (with CAIRO_PDO_USER and CAIRO_PDO_PASSWORD) picks the
// database; by default an SQLite database in memory. CAIRO_SQL_DSN adds
// /sql, the same loop through Cairo\Sql.

declare(strict_types=1);

use Cairo\App;
use Cairo\Http\Response;
use Cairo\Sql\Db;

use function Cairo\{async, await, all};

$app = new App();
$app->repeatedQueries(10);
$http = $app->http(getenv('CAIRO_HTTP') ?: ':8080');

$pdo = static fn (): PDO => new PDO(
    getenv('CAIRO_PDO_DSN') ?: 'sqlite::memory:',
    getenv('CAIRO_PDO_USER') ?: null,
    getenv('CAIRO_PDO_PASSWORD') ?: null,
    [PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION],
);

// The N+1: one query per order, as an ORM does when it loads a relation
// lazily. Half run in async() tasks, each with its own connection; they
// count for the same request.
$http->get('/orders', function () use ($pdo): Response {
    $load = static function (PDO $db, int $id): void {
        $db->prepare('SELECT ? AS order_id')->execute([$id]);
    };
    $db = $pdo();
    foreach (range(1, 6) as $id) {
        $load($db, $id);
    }
    await(all(array_map(static fn (int $id) => async(static fn () => $load($pdo(), $id)), range(7, 12))));
    return Response::text("12 orders\n");
});

// Literals typed into the statement: counted as one shape, and never logged.
$http->get('/inline', function () use ($pdo): Response {
    $db = $pdo();
    foreach (range(1, 10) as $i) {
        $db->query("SELECT $i AS n, 'secret-$i' AS s")->fetchAll();
    }
    return Response::text("10 queries\n");
});

// Under the threshold: never logged.
$http->get('/few', function () use ($pdo): Response {
    $db = $pdo();
    $db->exec('CREATE TEMPORARY TABLE seen (id INT)');
    foreach (range(1, 2) as $i) {
        $db->exec("INSERT INTO seen (id) VALUES ($i)");
    }
    return Response::text("3 statements\n");
});

if (($dsn = getenv('CAIRO_SQL_DSN')) !== false && $dsn !== '') {
    $sql = Db::open($dsn);
    $http->get('/sql', function () use ($sql): Response {
        foreach (range(1, 10) as $id) {
            $sql->query('SELECT $1::int AS id', [$id]);
        }
        return Response::text("10 queries\n");
    });
}

return $app;
