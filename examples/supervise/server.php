<?php

// Thread supervision: a PHP thread that dies (here, a fatal error from
// memory_limit) is restarted, and the requests it held get 500 instead of
// hanging. App::maxRequests() and App::maxMemory() replace a thread with a
// fresh one before leaks pile up, without failing any request.
// App::maxInFlight() caps requests per thread; the rest wait in Go.

declare(strict_types=1);

use Cairo\App;
use Cairo\Http\Response;

use function Cairo\async;
use function Cairo\await;
use function Cairo\sleep;

/** Changes every time a PHP thread runs server.php, so a test can see restarts. */
final class Boot
{
    public static string $id = '';
    /** @var list<string> grows on every /grow, to stand in for a leak */
    public static array $leak = [];
}
Boot::$id = bin2hex(random_bytes(6));

$app = new App();
$app->maxRequests((int) (getenv('CAIRO_MAX_REQUESTS') ?: 0))
    ->maxMemory(getenv('CAIRO_MAX_MEMORY') ?: 0)
    ->maxInFlight((int) (getenv('CAIRO_MAX_IN_FLIGHT') ?: 0));

$http = $app->http(getenv('CAIRO_HTTP') ?: ':8080');

$http->get('/boot', fn (): Response => Response::text(Boot::$id));

// Waits on Go, so it is in flight when another request kills the thread.
$http->get('/slow', function (): Response {
    sleep(1);
    return Response::text(Boot::$id);
});

// The same, from an async() task.
$http->get('/slow-async', function (): Response {
    return Response::text(await(async(function (): string {
        sleep(1);
        return Boot::$id;
    })));
});

// A fatal error: PHP ends the whole thread, not only this request.
$http->get('/crash', function (): Response {
    ini_set('memory_limit', '16M');
    $hog = [];
    while (true) {
        $hog[] = str_repeat('x', 1 << 20);
    }
});

$http->get('/grow', function (): Response {
    Boot::$leak[] = str_repeat('x', 4 << 20);
    return Response::text(Boot::$id);
});

return $app;
