<?php

// The heavy-route warning: a route that runs PHP for a long stretch without
// yielding is named in the log, since every other request on its thread
// waits meanwhile. Measured in Go, with no code in the handlers.
//
//     cairo serve server.php
//     curl localhost:8080/heavy
//
//     level=WARN msg="heavy route: move its CPU work to offload()" route="GET /heavy" longest_run_ms=80

declare(strict_types=1);

use Cairo\App;
use Cairo\Http\Response;

use function Cairo\{offload, sleep};

function burn(float $seconds): int
{
    $end = microtime(true) + $seconds;
    $n = 0;
    while (microtime(true) < $end) {
        $n++;
    }
    return $n;
}

$app = new App();
$app->heavyRoutes((float) (getenv('CAIRO_HEAVY') ?: 0.05));
$http = $app->http(getenv('CAIRO_HTTP') ?: ':8080');

// 80 ms of PHP in one stretch: logged.
$http->get('/heavy', fn (): Response => Response::text((string) burn(0.08)));

// The same 80 ms, in four stretches with a wait between them: each is under
// the threshold, so it is not logged.
$http->get('/yields', function (): Response {
    $n = 0;
    for ($i = 0; $i < 4; $i++) {
        $n += burn(0.02);
        sleep(0.005);
    }
    return Response::text((string) $n);
});

// The same 80 ms on an offload thread: the request thread is free meanwhile.
$http->get('/offloaded', fn (): Response => Response::text((string) offload('burn', 0.08)));

return $app;
