<?php

// The leak detector: logs static properties that hold one request's state
// but are not request-local, so another request on the thread would read it.
//
//     cairo serve server.php
//     curl 'localhost:8080/tenant?name=acme'; curl 'localhost:8080/tenant?name=globex'
//
//     level=WARN msg="static changed by a request" route="GET /tenant" static=Tenant::$current
//
// /visit (a list that grows) is reported too; /local (registered) and
// /config (a cache that gains keys) are not.

declare(strict_types=1);

use Cairo\App;
use Cairo\Http\Request;
use Cairo\Http\Response;
use Cairo\RequestLocal;

final class Tenant
{
    /** Request state in a plain static: the leak. */
    public static ?string $current = null;
    /** The same, but registered: each request has its own. */
    public static ?string $local = null;
    /** @var list<string> a list that grows with each request: a leak too */
    public static array $visitors = [];
    /** @var array<string, string> a cache: set once, or given new keys, and kept */
    private static array $config = [];

    /** @return array<string, string> */
    public static function config(string $key): array
    {
        self::$config[$key] ??= strtoupper($key);
        return self::$config;
    }
}

RequestLocal::register(Tenant::class, 'local');

$app = new App();
$app->detectLeaks();
if (getenv('CAIRO_ISOLATED')) {
    $app->concurrency('isolated');   // a static leaks from one request to the next there too
}
$http = $app->http(getenv('CAIRO_HTTP') ?: ':8080');

$http->get('/tenant', function (Request $req): Response {
    Tenant::$current = (string) $req->query('name');
    return Response::text(Tenant::$current . "\n");
});

$http->get('/local', function (Request $req): Response {
    Tenant::$local = (string) $req->query('name');
    return Response::text(Tenant::$local . "\n");
});

$http->get('/visit', function (Request $req): Response {
    Tenant::$visitors[] = (string) $req->query('name');
    return Response::json(Tenant::$visitors);
});

$http->get('/config', fn (Request $req): Response => Response::json(Tenant::config((string) $req->query('key'))));

return $app;
