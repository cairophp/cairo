<?php

// The slow request log: a request over the threshold is logged with where
// its time went, measured in Go, with no code in the handlers.
//
//     cairo serve server.php
//     curl 'localhost:8080/report?url=https://example.com'
//
//     level=WARN msg="slow request" route="GET /report" ms=283 php_ms=31 longest_run_ms=30 http="1 in 51ms" sleep="4 in 402ms"

declare(strict_types=1);

use Cairo\App;
use Cairo\Http\Client;
use Cairo\Http\Request;
use Cairo\Http\Response;

use function Cairo\{async, await, all, offload, sleep};

$app = new App();
$app->slowRequests((float) (getenv('CAIRO_SLOW') ?: 0.1));
$http = $app->http(getenv('CAIRO_HTTP') ?: ':8080');

// An HTTP call, three waits side by side in async() tasks, one wait of its
// own, and 30 ms of PHP.
$http->get('/report', function (Request $req): Response {
    (new Client(['timeout' => 5]))->get((string) $req->query('url'));
    await(all([
        async(fn () => sleep(0.1)),
        async(fn () => sleep(0.1)),
        async(fn () => sleep(0.1)),
    ]));
    sleep(0.1);
    $end = microtime(true) + 0.03;
    while (microtime(true) < $end) {
        // CPU work
    }
    return Response::text('done');
});

// PHP's own functions wait on the thread, with no goroutine, and are counted
// like the waits through Cairo's clients: a read of a socket that answers
// after 0.15 s (socket=) and a curl transfer (http=).
$http->get('/php-io', function (Request $req): Response {
    $conn = stream_socket_client('tcp://' . $req->query('addr'), $errno, $error, 5);
    if ($conn === false) {
        return Response::text("connect: $error", 502);
    }
    fwrite($conn, "0.15\n");
    fgets($conn);
    $curl = curl_init((string) $req->query('url'));
    curl_setopt($curl, CURLOPT_RETURNTRANSFER, true);
    curl_exec($curl);
    return Response::text('done');
});

// A job on an offload thread, which sleeps 0.15 s there: the request waits
// for it (offload=) and its own thread runs no PHP meanwhile.
$http->get('/offloaded', fn (): Response => Response::text((string) offload('usleep', 150000)));

// Fast: never logged.
$http->get('/fast', fn (): Response => Response::text('fast'));

return $app;
