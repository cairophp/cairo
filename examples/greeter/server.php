<?php

declare(strict_types=1);

use Cairo\App;
use Cairo\Context;
use Cairo\Grpc\Code;
use Cairo\Grpc\Exception as GrpcException;
use Cairo\Http\Request;
use Cairo\Http\Response;

use function Cairo\all;
use function Cairo\async;
use function Cairo\await;
use function Cairo\offload;
use function Cairo\sleep;

final class Greeter
{
    public function SayHello(array $req, Context $ctx): array
    {
        $name = $req['name'] ?? '';
        if ($name === '') {
            throw new GrpcException(Code::INVALID_ARGUMENT, 'name is required');
        }
        return ['message' => "Hello $name"];
    }

    public function SlowHello(array $req, Context $ctx): array
    {
        $remaining = $ctx->remaining();
        sleep((float) ($req['seconds'] ?? 0));   // the fiber suspends; Go waits
        return [
            'message' => 'Hello ' . ($req['name'] ?? ''),
            'remaining' => is_finite($remaining) ? $remaining : 0,
        ];
    }
}

/** CPU-bound work: spins for $seconds. Runs on the offload pool. */
function burn(float $seconds): int
{
    $end = microtime(true) + $seconds;
    $n = 0;
    while (microtime(true) < $end) {
        $n++;
    }
    return $n;
}

$app = new App();
$app->threads('auto');          // request threads: many fibers each
$app->offloadThreads('auto');   // CPU threads for Cairo\offload()

$http = $app->http(getenv('CAIRO_HTTP') ?: ':8080');

$http->get('/hello/{name}', fn (Request $req): Response => Response::text("Hello {$req->param('name')}\n"));

// Each request sleeps without blocking the thread: one PHP thread can hold
// thousands of these at once.
$http->get('/sleep', function (Request $req): Response {
    $seconds = (float) ($req->query('seconds') ?? 1);
    sleep($seconds);
    return Response::json(['slept' => $seconds]);
});

// Two waits run concurrently inside one request: about 1s in total, not 2s.
$http->get('/parallel', function (): Response {
    $start = microtime(true);
    [$a, $b] = await(all([
        async(function () { sleep(1); return 'a'; }),
        async(function () { sleep(1); return 'b'; }),
    ]));
    return Response::json(['results' => [$a, $b], 'took' => round(microtime(true) - $start, 2)]);
});

// CPU-heavy work goes to the offload pool; the request thread keeps serving
// other requests while this one waits.
$http->get('/cpu', function (Request $req): Response {
    $seconds = (float) ($req->query('seconds') ?? 1);
    return Response::json(['iterations' => offload('burn', $seconds)]);
});

$grpc = $app->grpc(getenv('CAIRO_GRPC') ?: ':9000')
    ->proto(__DIR__ . '/proto/helloworld.proto')
    ->maxDuration(30);

$grpc->service('helloworld.Greeter', Greeter::class);

return $app;
