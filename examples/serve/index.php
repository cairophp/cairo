<?php

// A file that serves its App itself instead of returning it, as a framework's
// runner does (cairophp/symfony-runtime serves Symfony's front controller
// this way), and then ends the script with exit(0):
//
//   cairo serve examples/serve/index.php

declare(strict_types=1);

use Cairo\App;
use Cairo\Http\Request;
use Cairo\Http\Response;

$app = (new App())->maxRequests((int) (getenv('MAX_REQUESTS') ?: 0));
$app->http(getenv('CAIRO_HTTP') ?: ':8080')
    ->get('/hello', fn (Request $req): Response => Response::text('Hello ' . ($req->query('name') ?? 'world') . "\n"));
$app->serve();
exit(0);
