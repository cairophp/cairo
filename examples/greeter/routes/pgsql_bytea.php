<?php

// bytea columns come back as read-only streams, as with pdo_pgsql.

declare(strict_types=1);

use Cairo\Http\Request;
use Cairo\Http\Response;

return function ($http): void {
    $http->get('/pgsql/bytea', function (Request $req): Response {
        $pdo = new PDO((string) getenv('CAIRO_TEST_PGSQL'), (string) getenv('CAIRO_TEST_PGSQL_USER'), (string) getenv('CAIRO_TEST_PGSQL_PASSWORD'));
        $st = $pdo->prepare('SELECT CAST(:b AS bytea) AS b, NULL::bytea AS n');
        $st->bindValue('b', "\x00\xffbin", PDO::PARAM_LOB);
        $st->execute();
        $row = $st->fetch(PDO::FETCH_ASSOC);
        $st->execute();
        $st->bindColumn('b', $asString, PDO::PARAM_STR);
        $st->fetch(PDO::FETCH_BOUND);
        return Response::json([
            'is_stream' => is_resource($row['b']),
            'hex' => bin2hex(stream_get_contents($row['b'])),
            'null' => $row['n'],
            'bound_string' => is_string($asString) ? bin2hex($asString) : gettype($asString),
        ]);
    });
};
