<?php

// Cairo\Channel and select(): fibers of one request (or of one thread)
// passing values, Go style.

declare(strict_types=1);

use Cairo\Channel;
use Cairo\Context;
use Cairo\Http\Request;
use Cairo\Http\Response;

use function Cairo\async;
use function Cairo\await;
use function Cairo\select;
use function Cairo\sleep;

if (!class_exists('ChannelSeen')) {
    /** What requests that were cut off saw, read back through /chan/seen. */
    final class ChannelSeen
    {
        /** @var array<string, string> */
        public static array $log = [];
    }
}

return function ($http): void {
    // A producer and a consumer over an unbuffered channel, then a buffered one.
    $http->get('/chan/pipeline', function (): Response {
        $ch = new Channel();
        $producer = async(function () use ($ch): void {
            foreach ([1, 2, 3, 4, 5] as $n) {
                $ch->send($n);
            }
            $ch->close();
        });
        $sum = 0;
        foreach ($ch as $n) {
            $sum += $n;
        }
        await($producer);

        $buf = new Channel(2);
        $sent = 0;
        while ($buf->trySend($sent)) {
            $sent++;
        }
        $buf->tryRecv($first);
        return Response::json(['sum' => $sum, 'buffered' => $sent, 'count' => count($buf), 'first' => $first]);
    });

    // Three producers that finish at different times; select() takes values
    // as they come.
    $http->get('/chan/fanin', function (): Response {
        $a = new Channel();
        $b = new Channel();
        $c = new Channel();
        foreach ([[$a, 0.3, 'a'], [$b, 0.1, 'b'], [$c, 0.2, 'c']] as [$ch, $delay, $name]) {
            async(function () use ($ch, $delay, $name): void {
                sleep($delay);
                $ch->send($name);
            });
        }
        $order = [];
        while (count($order) < 3) {
            [$key, $value] = select(['a' => $a, 'b' => $b, 'c' => $c]);
            $order[] = "$key=$value";
        }
        return Response::json($order);
    });

    // select() with a timeout, and with 0 (never waits).
    $http->get('/chan/timeout', function (): Response {
        $ch = new Channel();
        $start = microtime(true);
        [$key] = select(['x' => $ch], 0.2);
        $waited = microtime(true) - $start;
        [$now] = select(['x' => $ch], 0);
        return Response::json(['key' => $key, 'waited' => round($waited, 2), 'now' => $now]);
    });

    // A buffered channel keeps the order of its values, also those of
    // senders that waited for room; a receiver that waits gets its value
    // without the buffer; and a channel in use for long holds no more memory
    // than the values in it.
    $http->get('/chan/buffered', function (): Response {
        $ch = new Channel(3);
        $producer = async(function () use ($ch): void {
            foreach (range(1, 1000) as $n) {
                $ch->send($n);
            }
            $ch->close();
        });
        $inOrder = iterator_to_array($ch, false) === range(1, 1000);
        await($producer);

        $full = new Channel(2);
        $full->send('a');
        $full->send('b');
        $blocked = async(fn () => $full->send('c'));   // waits for room
        $refused = !$full->trySend('d');
        $taken = [$full->recv(), count($full), $full->recv(), $full->recv()];
        await($blocked);

        $direct = new Channel(2);
        $receiver = async(fn () => $direct->recv());   // waits for a value
        $direct->send('x');
        $handed = [count($direct), await($receiver)];

        $steady = new Channel(4);
        $steady->send(0);
        $steady->send(0);
        $before = memory_get_usage();
        $sum = 0;
        for ($i = 1; $i <= 200000; $i++) {
            $steady->send($i);
            $sum += $steady->recv();
        }
        return Response::json([
            'in_order' => $inOrder,
            'refused_when_full' => $refused,
            'taken' => $taken,
            'handed' => $handed,
            'steady' => [$sum, count($steady), $steady->tryRecv($next), $next, memory_get_usage() - $before < 65536],
        ]);
    });

    // Closed channels.
    $http->get('/chan/closed', function (): Response {
        $ch = new Channel(1);
        $ch->send('last');
        $ch->close();
        $out = ['drained' => $ch->recv()];
        try {
            $ch->recv();
        } catch (Channel\Closed $e) {
            $out['recv'] = $e->getMessage();
        }
        try {
            $ch->send('more');
        } catch (Channel\Closed $e) {
            $out['send'] = $e->getMessage();
        }
        [$key, $value, $ok] = select(['c' => $ch]);
        $out['select'] = [$key, $value, $ok];

        // A sender waiting when the channel closes gets Closed too.
        $unbuffered = new Channel();
        $task = async(fn () => $unbuffered->send('never taken'));
        $unbuffered->close();
        try {
            await($task);
        } catch (Channel\Closed) {
            $out['waiting_sender'] = 'closed';
        }
        return Response::json($out);
    });

    // A value handed over after 1 second: many of these at once on one PHP
    // thread take about 1 second in all.
    $http->get('/chan/handoff', function (): Response {
        $ch = new Channel();
        async(function () use ($ch): void {
            sleep(1);
            $ch->send('ok');
        });
        return Response::text($ch->recv());
    });

    // Waits for a value nobody sends, until the client goes away: once with
    // $ctx->done() as a case, once without (the wait throws).
    $http->get('/chan/until-done', function (Request $req, Context $ctx): Response {
        [$key, , $ok] = select(['never' => new Channel(), 'done' => $ctx->done()]);
        ChannelSeen::$log['until-done'] = $key . ($ok ? ':open' : ':closed') . ':' . get_class($ctx->err() ?? new stdClass());
        return Response::text('done');
    });
    $http->get('/chan/wait-forever', function (): Response {
        try {
            (new Channel())->recv();
        } catch (Context\Canceled $e) {
            ChannelSeen::$log['wait-forever'] = $e::class;
            throw $e;
        }
        return Response::text('unreachable');
    });
    $http->get('/chan/seen', fn (): Response => Response::json((object) ChannelSeen::$log));
};
