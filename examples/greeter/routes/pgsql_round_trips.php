<?php

// PDO pgsql runs a statement in one round trip unless the app asks for
// prepared statements on the server: Cairo makes
// PDO::PGSQL_ATTR_DISABLE_PREPARES the default of a new connection.

declare(strict_types=1);

use Cairo\Http\Request;
use Cairo\Http\Response;

return function ($http): void {
    // ?prepares=1 asks for server-side prepared statements in the options.
    $http->get('/pgsql/round-trips', function (Request $req): Response {
        $options = [PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION];
        if ($req->query('prepares') !== null) {
            $options[PDO::PGSQL_ATTR_DISABLE_PREPARES] = false;
        }
        $pdo = new PDO((string) getenv('CAIRO_TEST_PGSQL'), (string) getenv('CAIRO_TEST_PGSQL_USER'), (string) getenv('CAIRO_TEST_PGSQL_PASSWORD'), $options);
        $st = $pdo->prepare('SELECT CAST(? AS integer) + 1');
        $st->execute([41]);
        $answer = (int) $st->fetchColumn();
        // A statement prepared on the server stays there while $st lives.
        $prepared = (int) $pdo->query('SELECT count(*) FROM pg_prepared_statements')->fetchColumn();
        return Response::json([
            'answer' => $answer,
            'disable_prepares' => (bool) $pdo->getAttribute(PDO::PGSQL_ATTR_DISABLE_PREPARES),
            'prepared_on_server' => $prepared > 0,
        ]);
    });
};
