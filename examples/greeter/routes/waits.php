<?php

// Waits that stay on the PHP thread: sleep() and select() with their timers,
// and fibers that wake each other without ever waiting for Go.

declare(strict_types=1);

use Cairo\Channel;
use Cairo\Http\Request;
use Cairo\Http\Response;

use function Cairo\all;
use function Cairo\async;
use function Cairo\await;
use function Cairo\select;
use function Cairo\sleep;

return function ($http): void {
    // Tasks that sleep for different times wake in the order of those times.
    $http->get('/waits/order', function (): Response {
        $start = microtime(true);
        $woke = [];
        $tasks = [];
        foreach ([90, 10, 50, 30, 70, 20, 80, 40, 60, 0] as $ms) {
            $tasks[] = async(function () use ($ms, &$woke): void {
                sleep($ms / 1000);
                $woke[] = $ms;
            });
        }
        await(all($tasks));
        return Response::json(['woke' => $woke, 'took' => microtime(true) - $start]);
    });

    // ?n= tasks sleep 20 ms at once, twice over. Answers whether they woke
    // in the order they went to sleep, and how many fibers of the first
    // round ran a task of the second: a fiber whose task is done waits for
    // another one, up to 64 of them.
    $http->get('/waits/burst', function (Request $req): Response {
        $n = min(1000, max(1, (int) ($req->query('n') ?? '300')));
        $round = function () use ($n): array {
            $woke = [];
            $fibers = [];
            $tasks = [];
            for ($i = 0; $i < $n; $i++) {
                $tasks[] = async(function () use ($i, &$woke, &$fibers): void {
                    sleep(0.02);
                    $woke[] = $i;
                    $fibers[] = \Fiber::getCurrent();
                });
            }
            await(all($tasks));
            return [$woke === range(0, $n - 1), $fibers];
        };
        [$first, $before] = $round();
        [$second, $after] = $round();
        $reused = 0;
        foreach ($after as $fiber) {
            $reused += (int) in_array($fiber, $before, true);
        }
        return Response::json(['inOrder' => $first && $second, 'reused' => $reused]);
    });

    // Two fibers hand a number back and forth for ?ms= milliseconds. Neither
    // waits for Go, so the thread is never idle; and one of them waits with
    // a timeout of an hour that never passes, so every round leaves a timer
    // behind. Answers the rounds and how much memory they took.
    $http->get('/waits/ping-pong', function (Request $req): Response {
        $ping = new Channel();
        $pong = new Channel();
        async(function () use ($ping, $pong): void {
            while (true) {
                [, $n, $open] = select([$ping], 3600);
                if (!$open) {
                    return;
                }
                $pong->send($n + 1);
            }
        });
        $until = microtime(true) + (int) ($req->query('ms') ?? '500') / 1000;
        $before = memory_get_usage();
        $rounds = 0;
        while (microtime(true) < $until) {
            $ping->send($rounds);
            $rounds = $pong->recv();
        }
        $grew = memory_get_usage() - $before;
        $ping->close();
        return Response::json(['rounds' => $rounds, 'grew' => $grew]);
    });
};
