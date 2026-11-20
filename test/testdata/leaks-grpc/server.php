<?php

// For TestDetectLeaksGRPC: a gRPC service with the leak detector on. SayHello
// keeps the caller's name in two statics, and the app has the detector
// ignore one of them.

declare(strict_types=1);

use Cairo\App;
use Cairo\Context;

final class Seen
{
    public static string $ignored = '';
    public static string $last = '';
}

final class Greeter
{
    /**
     * @param array<string, mixed> $request
     * @return array<string, mixed>
     */
    public function SayHello(array $request, Context $ctx): array
    {
        Seen::$ignored = Seen::$last = (string) ($request['name'] ?? '');
        return ['message' => 'Hello ' . Seen::$last];
    }
}

$app = new App();
$app->detectLeaks(ignore: ['Seen::$ignored']);
$app->grpc(getenv('CAIRO_GRPC') ?: ':9000')
    ->proto(__DIR__ . '/../../../examples/greeter/proto/helloworld.proto')
    ->service('helloworld.Greeter', Greeter::class);

return $app;
