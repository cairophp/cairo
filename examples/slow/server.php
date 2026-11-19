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

use function Cairo\{async, await, all, sleep};

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

// Fast: never logged.
$http->get('/fast', fn (): Response => Response::text('fast'));

return $app;
