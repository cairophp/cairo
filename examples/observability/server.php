<?php

// What a Cairo process reports of itself: metrics for Prometheus, health
// and readiness probes, and OpenTelemetry traces.
//
//     OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 cairo serve server.php --drain 5s
//     curl localhost:8080/orders/7
//     curl localhost:9100/metrics    # cairo_http_request_duration_seconds, cairo_php_threads, ...
//     curl localhost:9100/readyz     # ok, and 503 from the stop signal on
//
// GET /greet calls this server's own gRPC service: the trace has the HTTP
// request, the client call under it and the gRPC call under that.

declare(strict_types=1);

use Cairo\App;
use Cairo\Context;
use Cairo\Http\Request;
use Cairo\Http\Response;
use Helloworld\GreeterClient;
use Helloworld\HelloReply;
use Helloworld\HelloRequest;

require __DIR__ . '/../greeter/gen/autoload.php';

final class TracedGreeter
{
    public function SayHello(HelloRequest $request, Context $ctx): HelloReply
    {
        return new HelloReply(message: "Hello {$request->name}, in trace " . explode('-', (string) $ctx->value('traceparent'))[1]);
    }
}

$app = new App();

// Spans go to the collector that OTEL_EXPORTER_OTLP_ENDPOINT names.
$app->tracing(service: 'orders');

$http = $app->http(getenv('CAIRO_HTTP') ?: ':8080');

// The span PHP runs under: pass it on in the calls PHP makes itself.
$http->get('/orders/{id}', fn (Request $req, Context $ctx): Response => Response::json([
    'order' => (int) $req->param('id'),
    'traceparent' => $ctx->value('traceparent'),
    'header' => $req->header('traceparent'),
]));

$http->get('/greet', function (Request $req): Response {
    $greeter = new GreeterClient((string) $req->query('target'), ['timeout' => 5]);
    return Response::text($greeter->SayHello(new HelloRequest(name: 'Cairo'))->message);
});

$http->get('/broken', fn (): Response => Response::text('no', 500));

$app->grpc(getenv('CAIRO_GRPC') ?: ':9000')
    ->proto(__DIR__ . '/../greeter/proto/helloworld.proto')
    ->service('helloworld.Greeter', TracedGreeter::class);

// The numbers and the probes, on a port of their own that only the
// monitoring reaches. health() also fits on the serving port, for a load
// balancer that probes there.
$app->http(getenv('CAIRO_OPS') ?: '127.0.0.1:9100')->metrics()->health();

return $app;
