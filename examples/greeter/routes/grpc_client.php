<?php

// Calling gRPC services from PHP: the generated typed client and the generic
// Cairo\Grpc\Client. These routes call this server's own Greeter.

declare(strict_types=1);

use Cairo\Grpc\Client;
use Cairo\Grpc\Code;
use Cairo\Grpc\Exception as GrpcException;
use Cairo\Http\Request;
use Cairo\Http\Response;
use Helloworld\GreeterClient;
use Helloworld\HelloRequest;
use Helloworld\SlowHelloRequest;

if (!function_exists('cairo_example_grpc_hello')) {
    /** Outside a fiber (offload thread): the thread waits for the call. */
    function cairo_example_grpc_hello(string $target, string $name): string
    {
        return (new GreeterClient($target))->SayHello(new HelloRequest(name: $name))->message;
    }
}

return function ($http): void {
    $http->get('/grpc-client/hello', function (Request $req): Response {
        $target = (string) $req->query('target');
        $greeter = new GreeterClient($target, ['timeout' => 5]);
        $out = ['typed' => $greeter->SayHello(new HelloRequest(name: (string) $req->query('name')))->message];

        $generic = new Client($target, ['protos' => [dirname(__DIR__) . '/proto/helloworld.proto'], 'metadata' => ['x-from' => 'cairo']]);
        $reply = $generic->invoke('/helloworld.Greeter/SayHello', ['name' => 'array']);
        $out['generic'] = $reply->message['message'];
        $out['has_content_type'] = isset($reply->headers['content-type']);

        $errors = [];
        try {
            $greeter->SlowHello(new SlowHelloRequest(name: 'late', seconds: 2), ['timeout' => 0.2]);
        } catch (GrpcException $e) {
            $errors['deadline'] = $e->getCode() === Code::DEADLINE_EXCEEDED;
        }
        try {
            $generic->call('/helloworld.Greeter/Missing', []);
        } catch (GrpcException $e) {
            $errors['unimplemented'] = $e->getCode() === Code::UNIMPLEMENTED;
        }
        try {
            (new Client('127.0.0.1:1', ['descriptors' => GreeterClient::DESCRIPTORS]))->call('/helloworld.Greeter/SayHello', ['name' => 'x'], ['timeout' => 2]);
        } catch (GrpcException $e) {
            $errors['unavailable'] = $e->getCode() === Code::UNAVAILABLE;
        }
        $out['errors'] = $errors;
        $out['offload'] = Cairo\offload('cairo_example_grpc_hello', $target, 'offload');
        return Response::json($out);
    });

    // One SlowHello; many of these overlap on one PHP thread.
    $http->get('/grpc-client/slow', function (Request $req): Response {
        $reply = (new GreeterClient((string) $req->query('target')))->SlowHello(new SlowHelloRequest(name: 'slow', seconds: 1));
        return Response::json(['message' => $reply->message]);
    });
};
