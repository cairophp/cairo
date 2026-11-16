<?php

// Cairo\Sql: PostgreSQL through Go's database/sql. The pool lives in Go and
// is shared by every PHP thread; queries suspend only the calling fiber.
//
//     CAIRO_SQL_DSN='postgres://app:secret@127.0.0.1/app' cairo serve server.php

declare(strict_types=1);

use Cairo\App;
use Cairo\Http\Request;
use Cairo\Http\Response;
use Cairo\Sql\Bytes;
use Cairo\Sql\Db;
use Cairo\Sql\SqlException;

$db = Db::open(getenv('CAIRO_SQL_DSN') ?: 'postgres://cairo:cairo@127.0.0.1:5432/cairo', [
    'max_open' => (int) (getenv('CAIRO_SQL_MAX_OPEN') ?: 10),
]);

$app = new App();
$http = $app->http(getenv('CAIRO_HTTP') ?: ':8080');

$http->post('/reset', function () use ($db): Response {
    $db->exec('DROP TABLE IF EXISTS cairo_sql_items');
    $db->exec('CREATE TABLE cairo_sql_items (id serial PRIMARY KEY, name text UNIQUE NOT NULL)');
    return Response::text('ok');
});

$http->post('/items', function (Request $req) use ($db): Response {
    $n = $db->exec('INSERT INTO cairo_sql_items (name) VALUES ($1)', [$req->query('name')]);
    return Response::json(['inserted' => $n]);
});

$http->get('/items', fn (): Response => Response::json(
    $db->query('SELECT id, name FROM cairo_sql_items ORDER BY id'),
));

// How PostgreSQL types come back.
$http->get('/types', function () use ($db): Response {
    $row = $db->queryRow(<<<'SQL'
        SELECT 42::int8 AS int, 1.5::float8 AS float, true AS bool, 'héllo'::text AS text,
               NULL::text AS null, '\x00ff10'::bytea AS bytes,
               '2026-01-02 03:04:05+00'::timestamptz AS time, 12.50::numeric AS numeric,
               '{"a": 1}'::jsonb AS json
        SQL);
    $row['bytes'] = bin2hex($row['bytes']);
    return Response::json($row);
});

// Parameters: a binary string, a date and an integer, sent and read back.
$http->get('/params', function () use ($db): Response {
    $row = $db->queryRow('SELECT $1::bytea AS b, $2::timestamptz AS t, $3::int8 + 1 AS n', [
        new Bytes("\x00\xff\x10"),
        new DateTimeImmutable('2026-05-06 07:08:09', new DateTimeZone('UTC')),
        9007199254740993,
    ]);
    return Response::json(['b' => bin2hex($row['b']), 't' => $row['t'], 'n' => $row['n']]);
});

// Committed: both rows stay. Rolled back: neither does.
$http->post('/tx', function (Request $req) use ($db): Response {
    $fail = $req->query('fail') === '1';
    try {
        $db->transaction(function ($tx) use ($fail): void {
            $tx->exec("INSERT INTO cairo_sql_items (name) VALUES ('tx-a')");
            $tx->exec("INSERT INTO cairo_sql_items (name) VALUES ('tx-b')");
            if ($fail) {
                throw new RuntimeException('changed my mind');
            }
        });
    } catch (RuntimeException $e) {
        // rolled back
    }
    return Response::json(['count' => $db->queryValue('SELECT count(*) FROM cairo_sql_items')]);
});

// Errors carry their SQLSTATE.
$http->get('/error', function (Request $req) use ($db): Response {
    try {
        $req->query('kind') === 'unique'
            ? $db->exec("INSERT INTO cairo_sql_items (name) VALUES ('dup'), ('dup')")
            : $db->query('SELECT * FROM no_such_table');
        return Response::text('no error', 500);
    } catch (SqlException $e) {
        return Response::json(['sqlstate' => $e->sqlState, 'message' => $e->getMessage()]);
    }
});

// Waits in Go: many of these run at once on one PHP thread.
$http->get('/sleep', fn (): Response => Response::text((string) $db->queryValue('SELECT 1 FROM pg_sleep(1)')));

$http->get('/stats', fn (): Response => Response::json($db->stats()));

return $app;
