<?php

// Derived contexts: withTimeout(), withCancel() and run(), done() of a
// derived context in select(), a Go operation that ends with its context,
// and values from Go middleware.

declare(strict_types=1);

use Cairo\Channel;
use Cairo\Context;
use Cairo\Http\Request;
use Cairo\Http\Response;

use function Cairo\async;
use function Cairo\await;
use function Cairo\Net\dial;
use function Cairo\select;
use function Cairo\sleep;

return function ($http): void {
    // A 0.2s budget for one step of the request; the request itself goes on.
    $http->get('/ctx/with-timeout', function (Request $req, Context $ctx): Response {
        $start = microtime(true);
        $child = $ctx->withTimeout(0.2);
        $out = $child->run(function (Context $c): string {
            try {
                sleep(1);
                return 'slept';
            } catch (Context\DeadlineExceeded) {
                return 'timed out';
            }
        });
        $took = microtime(true) - $start;
        sleep(0.05);   // the request's own context is still live
        return Response::json([
            'step' => $out,
            'took' => round($took, 2),
            'child_err' => $child->err() ? $child->err()::class : null,
            'parent_err' => $ctx->err() ? $ctx->err()::class : null,
            'child_remaining' => $child->remaining(),
            'nested' => $ctx->withTimeout(5)->withTimeout(0.1)->remaining() <= 0.1,
        ]);
    });

    // cancel() ends an async() task started under the context, and closes
    // its done() channel for a fiber waiting in select().
    $http->get('/ctx/cancel', function (Request $req, Context $ctx): Response {
        $c = $ctx->withCancel();
        $task = $c->run(fn () => async(function (): string {
            sleep(5);
            return 'finished';
        }));
        async(function () use ($c): void {
            sleep(0.1);
            $c->cancel();
        });
        $start = microtime(true);
        [$key, , $ok] = select(['never' => new Channel(), 'done' => $c->done()]);
        try {
            $result = await($task);
        } catch (Context\Canceled $e) {
            $result = $e::class;
        }
        try {
            $ctx->cancel();
        } catch (LogicException) {
            $cancelRequest = 'refused';
        }
        return Response::json([
            'select' => [$key, $ok],
            'waited' => round(microtime(true) - $start, 2),
            'task' => $result,
            'err' => $c->err() ? $c->err()::class : null,
            'parent_err' => $ctx->err() ? $ctx->err()::class : null,
            'cancel_request' => $cancelRequest ?? 'allowed',
        ]);
    });

    // done() of a derived context closes at its deadline, even for a fiber
    // waiting under the request's own context.
    $http->get('/ctx/done-deadline', function (Request $req, Context $ctx): Response {
        $d = $ctx->withTimeout(0.15);
        $start = microtime(true);
        [$key, , $ok] = select(['never' => new Channel(), 'done' => $d->done()]);
        return Response::json(['select' => [$key, $ok], 'waited' => round(microtime(true) - $start, 2)]);
    });

    // A Go operation in flight (here a read from ?addr=, a server that reads
    // a number of seconds, waits that long and answers "pong") ends with the
    // context it was asked under: at the deadline of
    // withTimeout(), or when another fiber calls cancel() on the context or
    // on one it derives from. Under a context that has ended it does not
    // start, and the request's own context goes on through all of it.
    $http->get('/ctx/go-operation', function (Request $req, Context $ctx): Response {
        $addr = (string) $req->query('addr');
        $ask = static function () use ($addr): string {
            $conn = dial('tcp', $addr);
            $conn->write("0.6\n");
            return trim((string) $conn->readLine());
        };
        $get = static function (Context $c) use ($ask): array {
            $start = microtime(true);
            try {
                $how = 'answered ' . $c->run($ask);
            } catch (Context\ContextException $e) {
                $how = $e::class;
            }
            return [$how, round(microtime(true) - $start, 2)];
        };
        $cancelSoon = static function (Context $c): Context {
            async(function () use ($c): void {
                sleep(0.1);
                $c->cancel();
            });
            return $c;
        };
        $deadline = $get($ctx->withTimeout(0.15));
        $canceled = $get($c = $cancelSoon($ctx->withCancel()));
        return Response::json([
            'deadline' => $deadline,
            'canceled' => $canceled,
            'ended' => $get($c),
            'parent_canceled' => $get($cancelSoon($ctx->withCancel())->withTimeout(30)),
            'request' => $get($ctx->withTimeout(30)->withCancel())[0],
            'request_err' => $ctx->err() ? $ctx->err()::class : null,
        ]);
    });

    // A task that nobody awaited does not wait on: the request's context ends
    // once the request is answered, and the task's sleep with it. The second
    // route tells how that sleep ended.
    $left = new Channel(1);
    $http->get('/ctx/left-running', function () use ($left): Response {
        async(function () use ($left): void {
            try {
                sleep(30);
                $left->trySend('slept');
            } catch (Context\Canceled $e) {
                $left->trySend($e::class);
            }
        });
        return Response::text("answered\n");
    });
    $http->get('/ctx/left-running/ended', function () use ($left): Response {
        [, $how] = select([$left], 5);
        return Response::json(['task' => $how]);
    });

    // A value set by Go middleware (Middleware::requestId()).
    $http->get('/ctx/value', fn (Request $req, Context $ctx): Response => Response::json([
        'request_id' => $ctx->value('request_id'),
        'header' => $req->header('X-Request-Id'),
        'missing' => $ctx->value('nope', 'default'),
    ]));
};
