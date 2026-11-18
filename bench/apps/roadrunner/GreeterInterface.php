<?php

// What protoc-gen-php-grpc would write for greeter.proto.

declare(strict_types=1);

namespace Rr\Helloworld;

use Spiral\RoadRunner\GRPC;

interface GreeterInterface extends GRPC\ServiceInterface
{
    public const NAME = 'helloworld.Greeter';

    public function SayHello(GRPC\ContextInterface $ctx, HelloRequest $in): HelloReply;
}
