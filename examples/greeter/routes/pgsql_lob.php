<?php

// PDO::pgsqlLOBCreate(), pgsqlLOBOpen() and pgsqlLOBUnlink(): large objects
// as PHP streams. They need a transaction.

declare(strict_types=1);

use Cairo\Http\Response;

return function ($http): void {
    $http->get('/pgsql/lob', function (): Response {
        $pdo = new PDO((string) getenv('CAIRO_TEST_PGSQL'), (string) getenv('CAIRO_TEST_PGSQL_USER'), (string) getenv('CAIRO_TEST_PGSQL_PASSWORD'), [PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION]);
        $data = "\0start\0" . random_bytes(100_000) . "\0end";

        // Write in chunks, then overwrite a few bytes after seeking back.
        $pdo->beginTransaction();
        $oid = $pdo->pgsqlLOBCreate();
        $lob = $pdo->pgsqlLOBOpen($oid, 'w');
        foreach (str_split($data, 30_000) as $chunk) {
            fwrite($lob, $chunk);
        }
        $written = ftell($lob);
        fseek($lob, 1);
        fwrite($lob, 'START');
        fclose($lob);
        $pdo->commit();
        $data = substr_replace($data, 'START', 1, 5);

        // Read it back whole, and a slice after seeking.
        $pdo->beginTransaction();
        $lob = $pdo->pgsqlLOBOpen($oid, 'rb');
        $back = stream_get_contents($lob);
        fseek($lob, 1);
        $slice = fread($lob, 5);
        fseek($lob, -3, SEEK_END);
        $tail = fread($lob, 10);
        fclose($lob);
        $pdo->commit();

        $unlinked = $pdo->pgsqlLOBUnlink($oid);
        $pdo->beginTransaction();
        try {
            $pdo->pgsqlLOBOpen($oid, 'rb');
            $missing = null;
        } catch (PDOException $e) {
            $missing = $e->getMessage();
        }
        $pdo->rollBack();
        // PHP refuses a name that is no OID before it asks the server where
        // libc's strtoul() says so (musl), and the server refuses OID 0
        // where it does not (glibc): either way nothing opens.
        try {
            $invalid = $pdo->pgsqlLOBOpen('not-an-oid') === false ? 'refused' : 'opened';
        } catch (PDOException) {
            $invalid = 'refused';
        }

        return Response::json([
            'oid_is_numeric' => preg_match('/^[1-9][0-9]*$/', $oid) === 1,
            'written' => $written,
            'same' => $back === $data,
            'length' => strlen($back),
            'slice' => $slice,
            'tail' => bin2hex($tail),
            'unlinked' => $unlinked,
            'missing' => $missing,
            'invalid' => $invalid,
            'usable' => $pdo->query('SELECT 1')->fetchColumn(),
        ]);
    });
};
