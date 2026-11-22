<?php

// Hard deadlines: Middleware::timeout() gives every request a deadline, and
// past it PHP code is stopped where it runs, even in a loop that never waits
// on I/O. The request gets 504 and the PHP thread is free again.

declare(strict_types=1);

use Cairo\App;
use Cairo\Context;
use Cairo\Http\Request;
use Cairo\Http\Response;
use Cairo\Middleware;

use function Cairo\async;
use function Cairo\await;

/** What the stopped requests saw, read back through /stats. */
final class Seen
{
    /** @var array<string, mixed> */
    public static array $log = [];
}

$app = new App();
$http = $app->http(getenv('CAIRO_HTTP') ?: ':8080')
    ->use(Middleware::timeout((float) (getenv('CAIRO_TIMEOUT') ?: 0.5)));

// A CPU loop that never suspends its fiber.
$http->get('/spin', function (): Response {
    $i = 0;
    try {
        while (true) {
            $i++;
        }
    } finally {
        Seen::$log['spin'] = $i > 0;
    }
});

// A loop that catches everything and carries on: still stopped. The second
// stop unwinds like exit(): catch and finally blocks are skipped.
$http->get('/spin-catch', function (): Response {
    $caught = 0;
    while (true) {
        try {
            for ($i = 0; $i < 1_000_000_000; $i++) {
            }
        } catch (\Throwable $e) {
            Seen::$log['spin_catch'] = ++$caught;
            Seen::$log['caught'] = get_class($e);
        }
    }
});

// The loop runs in an async() task; the request waits for it.
$http->get('/spin-async', function (Request $req, Context $ctx): Response {
    $task = async(function (): int {
        $n = 0;
        while (true) {
            $n = ($n + 1) % 1000;
        }
    });
    try {
        return Response::text((string) await($task));
    } catch (Context\DeadlineExceeded $e) {
        Seen::$log['async'] = get_class($e);
        throw $e;
    }
});

// An answer that comes after the deadline has nobody to go to: the client
// has its 504, and Cairo drops the answer, however large.
$http->get('/late', function (): Response {
    try {
        while (true) {
        }
    } catch (Context\DeadlineExceeded) {
        // Stopped by Cairo, so the request has ended on its side.
    }
    return Response::text(str_repeat('late ', 200_000));
});

// Fast requests are not touched.
$http->get('/ok', fn (): Response => Response::text("ok\n"));

$http->get('/stats', fn (): Response => Response::json(Seen::$log));

return $app;
