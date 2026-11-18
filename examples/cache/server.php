<?php

// Response cache: ->cache() lets Go answer repeated GET requests to a route
// without running PHP. Identical requests that arrive together make one PHP
// call, and a stale response is served while one request rebuilds it.
// Cache::purge() drops the responses of tagged routes when their data changes.

declare(strict_types=1);

use Cairo\App;
use Cairo\Cache;
use Cairo\Http\{Request, Response};

use function Cairo\sleep;

/** Counts the PHP calls of this thread, so a test can see what the cache saved. */
final class Calls
{
    public static int $n = 0;
}

$app = new App();
$http = $app->http(getenv('CAIRO_HTTP') ?: ':8080');

// Slow to build, the same for everyone: fresh for 1 s, then served stale for
// up to 10 s more while it is rebuilt.
$http->get('/products', function (Request $req): Response {
    $n = ++Calls::$n;
    sleep(0.2);
    return Response::json(['build' => $n, 'page' => $req->query('page') ?? '1']);
})->cache(ttl: 1, staleWhileRevalidate: 10);

// Varies on the language, so each Accept-Language gets its own entry.
$http->get('/hello', function (Request $req): Response {
    ++Calls::$n;
    $fr = str_starts_with($req->header('Accept-Language') ?? '', 'fr');
    return Response::text($fr ? 'bonjour' : 'hello');
})->cache(ttl: 60, vary: ['Accept-Language']);

// Sets a cookie, so Go never stores it even though the route opts in.
$http->get('/session', function (): Response {
    ++Calls::$n;
    return new Response(200, ['set-cookie' => 'sid=' . bin2hex(random_bytes(8))], 'new session');
})->cache(ttl: 60);

// Tagged: fresh for a minute, unless the POST below purges it first.
$http->get('/stock', function (): Response {
    return Response::json(['build' => ++Calls::$n]);
})->cache(ttl: 60, tags: ['stock']);

$http->post('/stock', function (): Response {
    Cache::purge('stock');
    return Response::text('purged');
});

$http->get('/calls', fn (): Response => Response::text((string) Calls::$n));

return $app;
