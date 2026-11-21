<?php

// Thread supervision: a PHP thread that dies (here, a fatal error from
// memory_limit) is restarted, and the requests it held get 500 instead of
// hanging. An offload thread that dies is restarted too, and the fiber that
// waits for the job it ran gets an OffloadException. App::maxRequests() and App::maxMemory() replace a thread with a
// fresh one before leaks pile up, without failing any request.
// App::maxInFlight() caps requests per thread; the rest wait in Go.

declare(strict_types=1);

use Cairo\App;
use Cairo\Http\Response;

use function Cairo\async;
use function Cairo\await;
use function Cairo\offload;
use function Cairo\sleep;

/** Changes every time a PHP thread runs server.php, so a test can see restarts. */
final class Boot
{
    public static string $id = '';
    /** @var list<string> grows on every /grow, to stand in for a leak */
    public static array $leak = [];
}
Boot::$id = bin2hex(random_bytes(6));

/** For offload(): which run of server.php the offload thread is. */
function boot_id(): string
{
    return Boot::$id;
}

/** A fatal error: PHP ends the whole thread that runs it. */
function exhaust_memory(): never
{
    ini_set('memory_limit', '16M');
    $hog = [];
    while (true) {
        $hog[] = str_repeat('x', 1 << 20);
    }
}

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
$http->get('/crash', fn (): Response => exhaust_memory());

$http->get('/offload/boot', fn (): Response => Response::text(offload('boot_id')));

// The same fatal error in an offloaded job: the offload thread ends, and
// this request, on its own thread, is told.
$http->get('/offload/crash', function (): Response {
    try {
        return Response::text((string) offload('exhaust_memory'));
    } catch (Cairo\OffloadException $e) {
        return Response::text($e::class . ': ' . $e->getMessage());
    }
});

$http->get('/grow', function (): Response {
    Boot::$leak[] = str_repeat('x', 4 << 20);
    return Response::text(Boot::$id);
});

return $app;
