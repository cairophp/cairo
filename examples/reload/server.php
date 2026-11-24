<?php

// Reloading the code of a running server.
//
//   cairo serve examples/reload/server.php --watch
//
// With --watch, Cairo looks at the files of the app and gives the server the
// new code when one changes: change greeting.php while this runs, and the
// next request has it. Without it, `kill -USR1 <pid>` does the same once, for
// a deploy. Either way no request is refused: each PHP thread is replaced by
// one that has read the code anew, and finishes the requests it holds
// (/slow) with the code they began with. Code that does not start (a parse
// error) replaces nothing.
//
// What this file declares (the servers, their routes, the settings) is built
// once, when the process starts. Change that, and --watch starts the
// process anew, while the signal says so in the log and changes nothing.

declare(strict_types=1);

use Cairo\App;
use Cairo\Http\Request;
use Cairo\Http\Response;
use Cairo\Http\Stream;

use function Cairo\offload;
use function Cairo\sleep;

require __DIR__ . '/greeting.php';

$app = new App();
if (getenv('CAIRO_CONCURRENCY') === 'isolated') {
    $app->concurrency('isolated');
}
// A thread that is recycled reads the code on disk too, and one that cannot
// start leaves the old thread in its place.
$app->maxRequests((int) (getenv('CAIRO_MAX_REQUESTS') ?: 0));

$http = $app->http(getenv('CAIRO_HTTP') ?: ':8080');

$http->get('/{$}', fn (): Response => Response::text(greeting()));

// Begins its answer at once and ends it after ?seconds=, with the code the
// request began with.
$http->get('/slow', fn (Request $req): Response => Response::stream(function (Stream $out) use ($req): void {
    $out->write('began ');
    sleep((float) ($req->query('seconds') ?? '1'));
    $out->write(greeting());
}, headers: ['Content-Type' => 'text/plain; charset=utf-8']));

// The offload threads get the new code too.
$http->get('/offload', fn (): Response => Response::text(offload('greeting')));

// Which directory this run of the file is in: a deploy that points a link
// at a new release and reloads is served from there.
$http->get('/where', fn (): Response => Response::text(basename(__DIR__)));

return $app;
