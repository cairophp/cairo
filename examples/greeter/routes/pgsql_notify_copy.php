<?php

// PDO::pgsqlGetNotify(), pgsqlGetPid() and the pgsqlCopy* methods.

declare(strict_types=1);

use Cairo\Http\Request;
use Cairo\Http\Response;

return function ($http): void {
    $connect = fn (): PDO => new PDO((string) getenv('CAIRO_TEST_PGSQL'), (string) getenv('CAIRO_TEST_PGSQL_USER'), (string) getenv('CAIRO_TEST_PGSQL_PASSWORD'), [PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION]);

    // LISTEN on one connection, NOTIFY from another; ?wait= milliseconds to
    // wait for a notification that is sent ?delay= milliseconds later.
    $http->get('/pgsql/notify', function (Request $req) use ($connect): Response {
        $channel = 'cairo_' . bin2hex(random_bytes(4));
        $listener = $connect();
        $listener->exec("LISTEN $channel");
        $before = $listener->pgsqlGetNotify(PDO::FETCH_ASSOC, 0);

        $sender = $connect();
        $sender->exec("NOTIFY $channel, 'hello'");
        $sender->exec("NOTIFY $channel");
        $first = $listener->pgsqlGetNotify(PDO::FETCH_ASSOC, 1000);
        $second = $listener->pgsqlGetNotify(PDO::FETCH_NUM, 1000);

        $delay = (int) ($req->query('delay') ?? '0');
        $wait = (int) ($req->query('wait') ?? '0');
        $late = null;
        if ($wait > 0) {
            $task = Cairo\async(function () use ($connect, $channel, $delay): void {
                usleep($delay * 1000);
                $connect()->exec("NOTIFY $channel, 'late'");
            });
            $start = microtime(true);
            $got = $listener->pgsqlGetNotify(PDO::FETCH_ASSOC, $wait);
            Cairo\await($task);
            $late = ['payload' => $got['payload'] ?? null, 'ms' => (int) round((microtime(true) - $start) * 1000)];
        }
        $timedOut = $listener->pgsqlGetNotify(PDO::FETCH_ASSOC, 50);

        return Response::json([
            'before' => $before,
            'first' => $first,
            'second' => $second,
            'sender_pid' => $sender->pgsqlGetPid(),
            'late' => $late,
            'timed_out' => $timedOut,
            'still_usable' => $listener->query('SELECT 1')->fetchColumn(),
        ]);
    });

    $http->get('/pgsql/copy', function (Request $req) use ($connect): Response {
        $pdo = $connect();
        $table = 'cairo_copy_' . bin2hex(random_bytes(4));
        $pdo->exec("CREATE TEMP TABLE $table (id int, name text, note text)");
        $in = $pdo->pgsqlCopyFromArray($table, ["1\tone\t\\N", "2\ttwo\tx\n"]);
        $semi = $pdo->pgsqlCopyFromArray($table, ['3;three;NULL'], ';', 'NULL', 'id, name, note');
        $file = tempnam(sys_get_temp_dir(), 'cairo-copy-');
        file_put_contents($file, "4\tfour\t\\N\n");
        $fromFile = $pdo->pgsqlCopyFromFile($table, $file);
        $rows = $pdo->pgsqlCopyToArray($table);
        $names = $pdo->pgsqlCopyToArray($table, ',', 'nil', 'id, note');
        $toFile = $pdo->pgsqlCopyToFile($table, $file, '|');
        $fileBody = file_get_contents($file);
        unlink($file);
        $error = null;
        try {
            $pdo->pgsqlCopyFromArray($table, ["not-a-number\tx\ty"]);
        } catch (PDOException $e) {
            $error = $e->getCode();
        }
        $pdo->setAttribute(PDO::ATTR_ERRMODE, PDO::ERRMODE_SILENT);
        $silent = $pdo->pgsqlCopyToArray('cairo_no_such_table');

        return Response::json([
            'in' => $in,
            'semi' => $semi,
            'from_file' => $fromFile,
            'rows' => $rows,
            'names' => $names,
            'to_file' => $toFile,
            'file' => $fileBody,
            'error' => $error,
            'silent' => $silent,
            'silent_code' => $pdo->errorCode(),
        ]);
    });
};
