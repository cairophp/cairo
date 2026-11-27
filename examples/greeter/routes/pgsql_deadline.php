<?php

// A deadline that passes in the middle of a PDO pgsql query ends the query's
// wait with Context\DeadlineExceeded. The connection is shut then: the server
// still answers the query, and that answer must not become the next one's.

declare(strict_types=1);

use Cairo\Context;
use Cairo\Http\Response;

return function ($http): void {
    $http->get('/pgsql/deadline', function (): Response {
        $pdo = new PDO((string) getenv('CAIRO_TEST_PGSQL'), (string) getenv('CAIRO_TEST_PGSQL_USER'), (string) getenv('CAIRO_TEST_PGSQL_PASSWORD'), [PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION]);
        $start = hrtime(true);
        try {
            Context::current()->withTimeout(0.2)->run(fn () => $pdo->query('SELECT pg_sleep(1)'));
            $cut = 'none';
        } catch (Context\DeadlineExceeded) {
            $cut = 'deadline';
        }
        $took = (hrtime(true) - $start) / 1e9;
        try {
            $next = 'answered ' . var_export($pdo->query('SELECT 7')->fetchColumn(), true);
        } catch (PDOException) {
            $next = 'failed';
        }
        return Response::json(['cut' => $cut, 'seconds' => round($took, 2), 'next' => $next]);
    });
};
