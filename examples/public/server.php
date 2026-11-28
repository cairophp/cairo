<?php

// A document root next to the app, like Laravel's public/: Go serves the
// files that exist, and every other path reaches PHP.

declare(strict_types=1);

use Cairo\App;
use Cairo\Http\Request;
use Cairo\Http\Response;

$app = new App();
$http = $app->http(getenv('CAIRO_HTTP') ?: ':8080');
$http->publicDir(__DIR__ . '/public');
// The same rules for files under a prefix, from a directory of their own.
$http->static('/assets', getenv('CAIRO_STATIC_DIR') ?: __DIR__ . '/public');
$http->fallback(static fn (Request $req): Response => Response::text('app ' . $req->path()));

return $app;
