<?php

// For the tests of opcache's restart (test/opcache_restart_test.go): threads
// that are recycled every few requests, a file of code that the test
// replaces to fill the cache (big.php, next to a copy of this file), and a
// route that resets the cache.

declare(strict_types=1);

use Cairo\App;
use Cairo\Http\Response;

if (is_file(__DIR__ . '/big.php')) {
    require __DIR__ . '/big.php';
}

$app = new App();
$app->maxRequests(10);

$http = $app->http(getenv('CAIRO_HTTP') ?: ':8080');

// Which version of big.php this thread has, after running some of it.
$http->get('/{$}', function (): Response {
    $version = 'none';
    for ($i = 0; function_exists("big_$i"); $i += 97) {
        $version = "big_$i"();
    }
    return Response::text($version);
});

$http->get('/reset', fn (): Response => Response::text(opcache_reset() ? 'reset' : 'not reset'));

return $app;
