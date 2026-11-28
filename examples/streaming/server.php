<?php

// Answers that are sent while their handler runs: a streamed body, Server-Sent
// Events and Early Hints; and files that Go sends for PHP. Each stream is a fiber: fifty clients that each
// wait a second between two pieces are served by one PHP thread in a second.

declare(strict_types=1);

use Cairo\App;
use Cairo\Context;
use Cairo\Http\Events;
use Cairo\Http\Request;
use Cairo\Http\Response;
use Cairo\Http\Stream;
use Cairo\Middleware;

use function Cairo\sleep;

/** What the streams saw of their clients, read back through /seen. */
final class Seen
{
    /** @var array<string, mixed> */
    public static array $log = [];
}

$app = new App();
$http = $app->http(getenv('CAIRO_HTTP') ?: ':8080');

// A time limit for requests, when the environment sets one: streams with
// noTimeout() outlast it.
if (($limit = getenv('CAIRO_TIMEOUT')) !== false) {
    $http->use(Middleware::timeout((float) $limit));
}
// The connection's own limits, when the environment sets them: 0 is none,
// and the head of a request and an idle connection have defaults.
$http->timeouts(
    write: (float) (getenv('CAIRO_WRITE_TIMEOUT') ?: 0),
    header: (float) (getenv('CAIRO_HEADER_TIMEOUT') ?: 10),
    idle: (float) (getenv('CAIRO_IDLE_TIMEOUT') ?: 120),
);

// The client has "first" a second before "second".
$http->get('/stream', fn (Request $req): Response => Response::stream(function (Stream $out) use ($req): void {
    $out->write("first\n");
    sleep((float) ($req->query('wait') ?? '1'));
    $out->write("second\n");
}, headers: ['Content-Type' => 'text/plain; charset=utf-8']));

// More than Go queues for a client: the writer waits for its reader.
$http->get('/large', fn (Request $req): Response => Response::stream(function (Stream $out) use ($req): void {
    $line = str_repeat('x', 1023) . "\n";
    try {
        for ($i = 0, $n = (int) ($req->query('kb') ?? '4096'); $i < $n; $i++) {
            $out->write($line);
        }
    } catch (Context\Canceled $e) {
        Seen::$log['large'] = 'the client stopped reading';
        throw $e;
    }
}, headers: ['Content-Type' => 'text/plain']));

// Server-Sent Events: new EventSource('/events') in a browser.
$http->get('/events', fn (Request $req): Response => Response::events(function (Events $events) use ($req): void {
    $from = (int) ($req->header('Last-Event-ID') ?? '0');
    $events->retry(2000);
    try {
        for ($i = $from + 1; $i <= $from + (int) ($req->query('count') ?? '3'); $i++) {
            $events->send(json_encode(['tick' => $i], JSON_THROW_ON_ERROR), event: 'tick', id: (string) $i);
            sleep(0.05);
        }
        $events->send("two\nlines");
    } catch (Context\Canceled $e) {
        Seen::$log['events'] = 'the client left';
        throw $e;
    }
}))->noTimeout();

// A stream that only ends when its client leaves.
$http->get('/forever', fn (): Response => Response::events(function (Events $events): void {
    try {
        while (true) {
            $events->comment('still here');
            sleep(0.05);
        }
    } finally {
        Seen::$log['forever'] = 'ended';
    }
}))->noTimeout();

// A handler that fails after its answer began: the client sees a broken answer.
$http->get('/fails', fn (): Response => Response::stream(function (Stream $out): void {
    $out->write("so far so good\n");
    throw new RuntimeException('failed halfway');
}));

// Early Hints: the browser starts on the stylesheet while the page is made.
$http->get('/hints', function (Request $req): Response {
    $req->earlyHints(['Link' => '</style.css>; rel=preload; as=style']);
    sleep(0.1);
    return Response::html('<link rel="stylesheet" href="/style.css"><p>Hello</p>');
});

// Work after the answer may wait as the handler may: here on a DNS lookup.
$http->get('/after', function (): Response {
    Context::defer(static function (): void {
        Seen::$log['after'] = gethostbyname('localhost');
    });
    return Response::text('answered');
});

// A body that is read while the client sends it: its size and a digest,
// with nothing of it kept in memory.
$http->post('/upload', function (Request $req): Response {
    $hash = hash_init('sha256');
    $bytes = 0;
    while (($piece = $req->read()) !== null) {
        hash_update($hash, $piece);
        $bytes += strlen($piece);
    }
    return Response::json(['bytes' => $bytes, 'sha256' => hash_final($hash), 'whole' => $req->body()]);
})->streamBody();

// The client speaks while it is answered: every line it sends comes back
// before it sends the next.
$http->post('/echo', fn (Request $req): Response => Response::stream(function (Stream $out) use ($req): void {
    while (($piece = $req->read()) !== null) {
        $out->write(strtoupper($piece));
    }
}))->streamBody()->noTimeout();

// A file that Go sends, with ranges, while the PHP thread goes on: named
// with Response::file(), or the way code written for nginx names it.
$http->sendfile(__DIR__ . '/files', '/protected');
$http->get('/file', fn (): Response => Response::file(__DIR__ . '/files/report.txt'));
$http->get('/file/accel', fn (): Response => new Response(200, ['X-Accel-Redirect' => '/protected/report.txt', 'Content-Type' => 'text/x-report']));
$http->get('/file/outside', fn (): Response => Response::file(__FILE__));

$http->get('/seen', fn (): Response => Response::json(Seen::$log));

return $app;
