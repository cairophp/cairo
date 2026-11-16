<?php

// Cairo\offload() with task objects: the job carries its own input.

declare(strict_types=1);

use Cairo\Http\Request;
use Cairo\Http\Response;
use Cairo\OffloadException;
use Cairo\OffloadTask;

use function Cairo\all;
use function Cairo\async;
use function Cairo\await;
use function Cairo\offload;

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

    final class FailingTask implements OffloadTask
    {
        public function run(): mixed
        {
            throw new DomainException('bad input');
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
        return Response::json([
            'same_as_local' => $results[0]['hash'] === $local['hash'],
            'distinct' => count(array_unique(array_column($results, 'hash'))) === 3,
            'modes' => array_column($results, 'thread_mode'),
            'error' => $error ?? null,
        ]);
    });
};
