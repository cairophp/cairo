<?php

// Dev-mode blocking detector: App::detectBlocking() logs, with a backtrace,
// every request that keeps its PHP thread busy longer than the threshold
// without yielding to Go. On a fiber thread that stalls every other request
// on it, so run with it on in development and fix what it reports.

declare(strict_types=1);

use Cairo\App;
use Cairo\Http\Request;
use Cairo\Http\Response;

use function Cairo\async;
use function Cairo\await;
use function Cairo\sleep;

$app = new App();
$app->detectBlocking((float) (getenv('CAIRO_DETECT_BLOCKING') ?: 0.1));

$http = $app->http(getenv('CAIRO_HTTP') ?: ':8080');

// Fine: sleep() yields, the thread serves others meanwhile.
$http->get('/ok', function (): Response {
    sleep(0.3);
    return Response::text('ok');
});

// Blocks: a CPU loop that never yields.
$http->get('/spin', function (): Response {
    $end = microtime(true) + 0.3;
    $n = 0;
    while (microtime(true) < $end) { $n++; } // BLOCKING-LINE
    return Response::text((string) $n);
});

// Fine too: Cairo hooks usleep(), sleep() and time_nanosleep(), so they yield.
$http->get('/usleep', function (): Response {
    usleep(300_000);
    return Response::text('slept');
});

// Fine: ten tasks each wait for a line from ?addr=, which come at the same
// moment, and then each computes for 30 ms. No fiber keeps the thread for
// long, and the thread comes back to its loop between them.
$http->get('/sockets', function (Request $req): Response {
    $tasks = [];
    for ($i = 0; $i < 10; $i++) {
        $conn = stream_socket_client('tcp://' . $req->query('addr'), $errno, $error, 5);
        if ($conn === false) {
            throw new RuntimeException("connect: $error");
        }
        fwrite($conn, "$i\n");
        $tasks[] = async(function () use ($conn): string {
            $line = (string) fgets($conn);
            $end = microtime(true) + 0.03;
            while (microtime(true) < $end) {
            }
            return $line;
        });
    }
    return Response::text(implode('', array_map(static fn ($task): string => await($task), $tasks)));
});

// Blocks: one long C call Cairo does not hook. The report comes as soon as
// PHP runs again after the call returns.
$http->get('/hash', function (): Response {
    return Response::text(password_hash('secret', PASSWORD_BCRYPT, ['cost' => 12]));
});

return $app;
