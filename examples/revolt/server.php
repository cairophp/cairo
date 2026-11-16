<?php

// Amp v3 and Revolt on Cairo. Cairo\Revolt\Driver makes Revolt's event loop
// run on Cairo's scheduler: Amp's delay(), async(), futures and cancellations,
// and Revolt's timers and stream watchers, wait in Go like any other Cairo
// call, so one PHP thread serves many such requests at once.
//
//     composer install && cairo serve server.php

declare(strict_types=1);

use Cairo\App;
use Cairo\Http\Request;
use Cairo\Http\Response;
use Revolt\EventLoop;

require __DIR__ . '/vendor/autoload.php';

Cairo\Revolt\Driver::install();

$app = new App();
$listen = getenv('CAIRO_HTTP') ?: ':8080';
$http = $app->http($listen);

// Three Amp tasks of 1s each, awaited together: 1s in all.
$http->get('/amp/parallel', function (): Response {
    $start = microtime(true);
    $results = Amp\Future\await([
        Amp\async(function (): string { Amp\delay(1); return 'a'; }),
        Amp\async(function (): string { Amp\delay(1); return 'b'; }),
        Amp\async(function (): string { Amp\delay(1); return 'c'; }),
    ]);
    ksort($results);   // keyed in the order they finished
    return Response::json(['results' => array_values($results), 'took' => round(microtime(true) - $start, 2)]);
});

// One Amp delay per request: many requests wait at once on one thread.
$http->get('/amp/delay', function (Request $req): Response {
    Amp\delay((float) ($req->query('s') ?? 1));
    return Response::text('ok');
});

// Amp's cancellations: a 5s delay canceled after 0.2s.
$http->get('/amp/timeout', function (): Response {
    $start = microtime(true);
    try {
        Amp\delay(5, cancellation: new Amp\TimeoutCancellation(0.2));
        $outcome = 'not canceled';
    } catch (Amp\CancelledException) {
        $outcome = 'canceled';
    }
    return Response::json(['outcome' => $outcome, 'took' => round(microtime(true) - $start, 2)]);
});

// A DeferredFuture completed by a timer callback.
$http->get('/amp/deferred', function (): Response {
    $deferred = new Amp\DeferredFuture();
    EventLoop::delay(0.1, fn () => $deferred->complete('done'));
    return Response::text($deferred->getFuture()->await());
});

// Revolt's callback types, in the order they ran.
$http->get('/revolt/order', function (): Response {
    $log = [];
    $suspension = EventLoop::getSuspension();
    EventLoop::delay(0.05, function () use (&$log): void { $log[] = 'delay'; });
    EventLoop::defer(function () use (&$log): void { $log[] = 'defer'; });
    EventLoop::queue(function () use (&$log): void { $log[] = 'queue'; });
    $ticks = 0;
    EventLoop::repeat(0.02, function (string $id) use (&$log, &$ticks): void {
        $log[] = 'repeat';
        if (++$ticks === 3) {
            EventLoop::cancel($id);
        }
    });
    $canceled = EventLoop::delay(0.01, function () use (&$log): void { $log[] = 'canceled timer ran'; });
    EventLoop::cancel($canceled);
    $disabled = EventLoop::delay(0.01, function () use (&$log): void { $log[] = 're-enabled'; });
    EventLoop::disable($disabled);
    EventLoop::delay(0.12, function () use ($disabled): void { EventLoop::enable($disabled); });
    EventLoop::delay(0.2, fn () => $suspension->resume());
    $log[] = 'sync';
    $suspension->suspend();
    return Response::json($log);
});

// onReadable/onWritable on a socket pair: the reader wakes when the writer,
// itself waiting for onWritable, sends.
$http->get('/revolt/stream', function (): Response {
    [$a, $b] = stream_socket_pair(STREAM_PF_UNIX, STREAM_SOCK_STREAM, STREAM_IPPROTO_IP);
    stream_set_blocking($a, false);
    stream_set_blocking($b, false);
    $suspension = EventLoop::getSuspension();
    $got = '';
    EventLoop::onReadable($a, function (string $id, $stream) use (&$got, $suspension): void {
        $got .= fread($stream, 1024);
        if (str_ends_with($got, "\n")) {
            EventLoop::cancel($id);
            $suspension->resume();
        }
    });
    EventLoop::delay(0.1, function () use ($b): void {
        EventLoop::onWritable($b, function (string $id, $stream): void {
            EventLoop::cancel($id);
            fwrite($stream, "hello over a socket\n");
        });
    });
    $suspension->suspend();
    fclose($a);
    fclose($b);
    return Response::text(trim($got));
});

// Amp's HTTP client: 20 requests at once to a 1s endpoint of this server,
// on the same PHP thread, take about 1s. (league/uri needs ext/filter.)
$http->get('/amp/http', function () use ($listen): Response {
    if (!extension_loaded('filter')) {
        return Response::text('this PHP build has no ext/filter', 501);
    }
    $client = Amp\Http\Client\HttpClientBuilder::buildDefault();
    $url = 'http://' . (str_starts_with($listen, ':') ? '127.0.0.1' : '') . $listen . '/amp/delay?s=1';
    $start = microtime(true);
    $bodies = Amp\Future\await(array_map(
        fn () => Amp\async(fn () => $client->request(new Amp\Http\Client\Request($url))->getBody()->buffer()),
        range(1, 20),
    ));
    return Response::json(['bodies' => implode('', $bodies), 'took' => round(microtime(true) - $start, 2)]);
});

// A callback that throws goes to the error handler.
$http->get('/revolt/error', function (): Response {
    $suspension = EventLoop::getSuspension();
    $previous = EventLoop::getErrorHandler();
    EventLoop::setErrorHandler(fn (Throwable $e) => $suspension->resume($e->getMessage()));
    try {
        EventLoop::defer(function (): void {
            throw new RuntimeException('thrown in a callback');
        });
        return Response::text($suspension->suspend());
    } finally {
        EventLoop::setErrorHandler($previous);
    }
});

return $app;
