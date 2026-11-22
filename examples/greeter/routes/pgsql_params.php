<?php

// What the parameters of a prepared statement carry to PostgreSQL: an empty
// string is not NULL, and text and bytes arrive as they are. With ?offload
// the same runs on an offload thread, which waits for each answer where a
// request's fiber suspends.

declare(strict_types=1);

use Cairo\Http\Request;
use Cairo\Http\Response;

if (!function_exists('cairo_example_pgsql_params')) {
    /** @return array<string, mixed> */
    function cairo_example_pgsql_params(): array
    {
        $pdo = new PDO((string) getenv('CAIRO_TEST_PGSQL'), (string) getenv('CAIRO_TEST_PGSQL_USER'), (string) getenv('CAIRO_TEST_PGSQL_PASSWORD'), [PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION]);
        $text = str_repeat("quote ' backslash \\ \"double\" tab\t newline\n é ع 🙂 ", 2000);
        $bytes = implode('', array_map('chr', range(0, 255)));
        $st = $pdo->prepare(
            'SELECT CAST(:empty AS text) AS empty, CAST(:null AS text) IS NULL AS null_is_null, md5(CAST(:text AS text)) AS text,'
            . " encode(CAST(:bytes AS bytea), 'hex') AS bytes, octet_length(CAST(:none AS bytea)) AS no_bytes"
        );
        $st->bindValue('empty', '');
        $st->bindValue('null', null, PDO::PARAM_NULL);
        $st->bindValue('text', $text);
        $st->bindValue('bytes', $bytes, PDO::PARAM_LOB);
        $st->bindValue('none', '', PDO::PARAM_LOB);
        $st->execute();
        $row = $st->fetch(PDO::FETCH_ASSOC);
        return [
            'empty' => $row['empty'],
            'null_is_null' => $row['null_is_null'],
            'text' => $row['text'] === md5($text),
            'bytes' => $row['bytes'] === bin2hex($bytes),
            'no_bytes' => $row['no_bytes'],
        ];
    }
}

return function ($http): void {
    $http->get('/pgsql/params', function (Request $req): Response {
        $onOffloadThread = $req->query('offload') !== null;
        return Response::json($onOffloadThread ? Cairo\offload('cairo_example_pgsql_params') : cairo_example_pgsql_params());
    });
};
