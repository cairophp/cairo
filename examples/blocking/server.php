<?php

// Dev-mode blocking detector: App::detectBlocking() logs, with a backtrace,
// every request that keeps its PHP thread busy longer than the threshold
// without yielding to Go. On a fiber thread that stalls every other request
// on it, so run with it on in development and fix what it reports.

declare(strict_types=1);

use Cairo\App;
use Cairo\Http\Response;

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

// Blocks: one long C call Cairo does not hook. The report comes as soon as
// PHP runs again after the call returns.
$http->get('/hash', function (): Response {
    return Response::text(password_hash('secret', PASSWORD_BCRYPT, ['cost' => 12]));
});

return $app;
