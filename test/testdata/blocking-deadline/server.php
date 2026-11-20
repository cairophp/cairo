<?php

// For TestBlockingThenDeadline: the blocking detector on, and a second
// server whose requests have a deadline that passes while their handler runs.

declare(strict_types=1);

use Cairo\App;
use Cairo\Http\Response;
use Cairo\Middleware;

$app = new App();
$app->detectBlocking(0.3);

// Blocks: reported once each time.
$app->http(getenv('CAIRO_HTTP') ?: ':8080')->get('/spin', function (): Response {
    $end = microtime(true) + 0.6;
    while (microtime(true) < $end) {
    }
    return Response::text("done\n");
});

// Stopped at its deadline, long before the detector's threshold.
$app->http(getenv('CAIRO_HTTP_TIMED') ?: ':8081')
    ->use(Middleware::timeout(0.05))
    ->get('/late', function (): Response {
        while (true) {
        }
    });

return $app;
