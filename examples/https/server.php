<?php

// HTTPS and HTTP/2 from Cairo itself, with no proxy in front.
//
// With certificate files:
//   CAIRO_TLS_CERT=cert.pem CAIRO_TLS_KEY=key.pem cairo serve examples/https/server.php
// With certificates from Let's Encrypt, on a machine the domain points to:
//   CAIRO_HTTP=:443 CAIRO_REDIRECT=:80 CAIRO_TLS_DOMAINS=example.com \
//   CAIRO_TLS_CACHE=/var/lib/cairo/certs cairo serve examples/https/server.php
// Behind a load balancer that speaks HTTP/2 without TLS:
//   CAIRO_H2C=1 cairo serve examples/https/server.php

declare(strict_types=1);

use Cairo\App;
use Cairo\Http\Request;
use Cairo\Http\Response;

$env = static fn (string $name): string => (string) getenv($name);

$app = new App();
$http = $app->http($env('CAIRO_HTTP') ?: ':8443');
if ($env('CAIRO_TLS_DOMAINS') !== '') {
    $http->autoTls(
        explode(',', $env('CAIRO_TLS_DOMAINS')),
        $env('CAIRO_TLS_CACHE') ?: __DIR__ . '/certs',
        $env('CAIRO_TLS_EMAIL'),
        $env('CAIRO_ACME_DIRECTORY'),
    );
} elseif ($env('CAIRO_TLS_CERT') !== '') {
    $http->tls($env('CAIRO_TLS_CERT'), $env('CAIRO_TLS_KEY'));
}
if ($env('CAIRO_REDIRECT') !== '') {
    $http->redirectHttp($env('CAIRO_REDIRECT'));
}
if ($env('CAIRO_H2C') !== '') {
    $http->h2c();
}

$http->get('/', static fn (Request $req): Response => Response::text(
    $req->protocol() . ($req->secure() ? ' with TLS' : ' without TLS')
));
// One HTTP/2 connection carries many requests at once, and each one that
// waits holds a fiber, not a thread.
$http->get('/wait', static function (): Response {
    sleep(1);
    return Response::text('waited');
});

if ($env('CAIRO_GRPC') !== '') {
    $app->grpc($env('CAIRO_GRPC'))->tls($env('CAIRO_TLS_CERT'), $env('CAIRO_TLS_KEY'));
}

return $app;
