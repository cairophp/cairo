<?php

// Cairo\Pool and cancellation: a request cancelled while it holds a
// connection drops it instead of giving it back, since the wait it was in
// may have cut a reply short.

declare(strict_types=1);

use Cairo\Http\Request;
use Cairo\Http\Response;
use Cairo\Pool;

use function Cairo\sleep;

return function ($http): void {
    $opened = new stdClass();
    $opened->n = 0;
    $pool = new Pool(function () use ($opened): stdClass {
        $opened->n++;
        return new stdClass();
    }, max: 1);

    // Holds the connection for 5s: with() for ?mode=with, get() otherwise.
    $http->get('/pool/hold', function (Request $req) use ($pool): Response {
        if ($req->query('mode') === 'with') {
            $pool->with(fn () => sleep(5));
        } else {
            $pool->get();
            sleep(5);
        }
        return Response::text('held');
    });

    $http->get('/pool/hold-stats', fn (): Response => Response::json(['opened' => $opened->n] + $pool->stats()));
};
