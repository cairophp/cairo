<?php

// Fair dispatch: a request that keeps its thread busy (a CPU loop here)
// does not get new requests queued behind it. Go sends them to a thread
// whose PHP is waiting for work or for I/O.
//
//     cairo serve --threads 2 server.php

declare(strict_types=1);

use Cairo\App;
use Cairo\Http\Request;
use Cairo\Http\Response;

use function Cairo\sleep;

$app = new App();
$http = $app->http(getenv('CAIRO_HTTP') ?: ':8080');

// Keeps its thread busy for ?ms= milliseconds without yielding.
$http->get('/spin', function (Request $req): Response {
    $end = microtime(true) + (int) ($req->query('ms') ?? 200) / 1000;
    $n = 0;
    while (microtime(true) < $end) {
        $n++;
    }
    return Response::text((string) $n);
});

// A light request: a short wait on I/O, then an answer.
$http->get('/light', function (): Response {
    sleep(0.005);
    return Response::text('light');
});

return $app;
