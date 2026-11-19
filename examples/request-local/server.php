<?php

// Request-local statics: Tenant::$current holds a separate value for each
// request, though many requests run on one PHP thread at once.
//
//     cairo serve server.php
//     curl 'localhost:8080/tenant?name=acme'

declare(strict_types=1);

use Cairo\App;
use Cairo\Http\Request;
use Cairo\Http\Response;
use Cairo\RequestLocal;

use function Cairo\{async, await, sleep};

final class Tenant
{
    public static string $current = 'none';
    /** @var list<string> */
    private static array $seen = [];

    /** @return list<string> */
    public static function seen(string $name): array
    {
        self::$seen[] = $name;
        return self::$seen;
    }
}

Tenant::$current = 'boot';   // what each request starts with
RequestLocal::register(Tenant::class, 'current', 'seen');

$app = new App();
$http = $app->http(getenv('CAIRO_HTTP') ?: ':8080');

// Sets the tenant, waits while other requests run on the thread, then
// reads it back from an async() task: always its own.
$http->get('/tenant', function (Request $req): Response {
    $before = Tenant::$current;
    $name = (string) $req->query('name');
    Tenant::$current = $name;
    $seen = Tenant::seen($name);
    sleep(0.2);
    $after = await(async(fn (): string => Tenant::$current));
    return Response::json(['before' => $before, 'after' => $after, 'seen' => $seen]);
});

return $app;
