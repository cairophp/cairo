<?php

// Cairo\offload() with task objects: the job carries its own input. A job
// goes from the request thread straight to an offload thread and its result
// straight back, so the routes below also show what the fiber that waits
// sees: each result reaches the fiber that asked, and the wait ends with the
// context it is under.

declare(strict_types=1);

use Cairo\Context;
use Cairo\Http\Request;
use Cairo\Http\Response;
use Cairo\OffloadException;
use Cairo\OffloadTask;

use function Cairo\all;
use function Cairo\async;
use function Cairo\await;
use function Cairo\offload;
use function Cairo\sleep;

if (!class_exists('HashChain')) {
    /** CPU work: hash $seed $rounds times. */
    final class HashChain implements OffloadTask
    {
        public function __construct(private string $seed, private int $rounds)
        {
        }

        public function run(): array
        {
            $h = $this->seed;
            for ($i = 0; $i < $this->rounds; $i++) {
                $h = hash('sha256', $h);
            }
            return ['hash' => $h, 'thread_mode' => json_decode(cairo_info(), true)['mode']];
        }
    }

    /** A job that takes $seconds of its thread. */
    final class Nap implements OffloadTask
    {
        public function __construct(private float $seconds)
        {
        }

        public function run(): string
        {
            usleep((int) ($this->seconds * 1e6));
            return 'done';
        }
    }

    final class FailingTask implements OffloadTask
    {
        public function run(): mixed
        {
            throw new DomainException('bad input');
        }
    }

    /** Its result cannot leave the offload thread: a closure is not serializable. */
    final class ClosureTask implements OffloadTask
    {
        public function run(): Closure
        {
            return static fn (): int => 1;
        }
    }
}

return function ($http): void {
    $http->get('/offload/task', function (Request $req): Response {
        $rounds = (int) ($req->query('rounds') ?? 1000);
        // Three tasks at once, then the same work on this thread to compare.
        $results = await(all([
            async(fn () => offload(new HashChain('a', $rounds))),
            async(fn () => offload(new HashChain('b', $rounds))),
            async(fn () => offload(new HashChain('c', $rounds))),
        ]));
        $local = (new HashChain('a', $rounds))->run();
        try {
            offload(new FailingTask());
        } catch (OffloadException $e) {
            $error = $e->getMessage();
        }
        try {
            offload(new ClosureTask());
        } catch (OffloadException $e) {
            $unserializable = $e->getMessage();
        }
        return Response::json([
            'same_as_local' => $results[0]['hash'] === $local['hash'],
            'distinct' => count(array_unique(array_column($results, 'hash'))) === 3,
            'modes' => array_column($results, 'thread_mode'),
            'error' => $error ?? null,
            'unserializable' => $unserializable ?? null,
        ]);
    });

    // ?n= jobs at once from one request: each fiber gets the result of its
    // own job, whichever offload thread ran it and in whatever order. With
    // ?busy= a job of that many seconds goes first, so that the others queue
    // behind it; past what the queue of the offload threads holds, a job
    // waits on its own thread for room.
    $http->get('/offload/many', function (Request $req): Response {
        $busy = async(static fn () => offload(new Nap((float) ($req->query('busy') ?? 0))));
        $seeds = range(1, (int) ($req->query('n') ?? 100));
        $hashes = await(all(array_map(
            static fn (int $seed) => async(static fn () => offload(new HashChain("seed $seed", 3))['hash']),
            $seeds,
        )));
        $right = 0;
        foreach ($seeds as $i => $seed) {
            $right += (int) ($hashes[$i] === (new HashChain("seed $seed", 3))->run()['hash']);
        }
        return Response::json(['jobs' => count($seeds), 'right' => $right, 'busy' => await($busy)]);
    });

    // The wait for a job ends with the context it is under: at its deadline,
    // or when another fiber cancels it; under one that has ended, the job is
    // not sent. Each case answers how the wait for a job of 0.4s ended and
    // after how long. The jobs themselves run on.
    $http->get('/offload/context', function (Request $req, Context $ctx): Response {
        $wait = static function (Context $c): array {
            $start = microtime(true);
            try {
                return [$c->run(static fn () => offload(new Nap(0.4))), round(microtime(true) - $start, 2)];
            } catch (Context\ContextException $e) {
                return [$e::class, round(microtime(true) - $start, 2)];
            }
        };
        $cancelSoon = static function (Context $c): Context {
            async(static function () use ($c): void {
                sleep(0.1);
                $c->cancel();
            });
            return $c;
        };
        $deadline = $wait($ctx->withTimeout(0.15));
        $canceled = $wait($c = $cancelSoon($ctx->withCancel()));
        return Response::json([
            'deadline' => $deadline,
            'canceled' => $canceled,
            'ended' => $wait($c),
            'live' => $wait($ctx->withTimeout(30)),
        ]);
    });
};
