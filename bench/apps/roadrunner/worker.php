<?php

// RoadRunner worker: HTTP through PSR-7, gRPC through spiral/roadrunner-grpc.
// RR_MODE says which pool started this process.

declare(strict_types=1);

use Nyholm\Psr7\Factory\Psr17Factory;
use Rr\Helloworld\HelloReply;
use Rr\Helloworld\HelloRequest;
use Spiral\RoadRunner\GRPC;
use Spiral\RoadRunner\Http\PSR7Worker;
use Spiral\RoadRunner\Worker;

require __DIR__ . '/vendor/autoload.php';
require __DIR__ . '/../router.php';

if (getenv('RR_MODE') === 'grpc') {
    $greeter = new class implements Rr\Helloworld\GreeterInterface {
        public function SayHello(GRPC\ContextInterface $ctx, HelloRequest $in): HelloReply
        {
            return (new HelloReply())->setMessage('Hello ' . $in->getName());
        }
    };
    $server = new GRPC\Server();
    $server->registerService(Rr\Helloworld\GreeterInterface::class, $greeter);
    $server->serve(Worker::create());
    return;
}

$factory = new Psr17Factory();
$worker = new PSR7Worker(Worker::create(), $factory, $factory, $factory);
while ($request = $worker->waitRequest()) {
    try {
        [$status, $type, $body] = bench_route($request->getUri()->getPath());
        $worker->respond($factory->createResponse($status)
            ->withHeader('Content-Type', $type)
            ->withBody($factory->createStream($body)));
    } catch (Throwable $e) {
        $worker->respond($factory->createResponse(500)->withBody($factory->createStream((string) $e)));
    }
}
