<?php

// Framework code on Cairo: a PSR-15 handler (what Slim, Mezzio and other
// PSR-15 stacks are) and a Symfony-style kernel, given to Cairo as handlers.
// A real Symfony app passes its Kernel, a Laravel app its HTTP Kernel:
//
//   $http->fallback($symfonyKernel);
//   $http->fallback($laravel->make(Illuminate\Contracts\Http\Kernel::class));
//
// Run `composer install` here first.

declare(strict_types=1);

use Cairo\App;
use Cairo\Http\Adapter;
use Nyholm\Psr7\Response as Psr7Response;
use Psr\Http\Message\ResponseInterface;
use Psr\Http\Message\ServerRequestInterface;
use Psr\Http\Message\UploadedFileInterface;
use Psr\Http\Server\RequestHandlerInterface;
use Symfony\Component\HttpFoundation\BinaryFileResponse;
use Symfony\Component\HttpFoundation\Cookie;
use Symfony\Component\HttpFoundation\JsonResponse;
use Symfony\Component\HttpFoundation\Request as SymfonyRequest;
use Symfony\Component\HttpFoundation\Response as SymfonyResponse;
use Symfony\Component\HttpFoundation\StreamedResponse;

require __DIR__ . '/vendor/autoload.php';

/** Echoes what it got as PSR-7, so a test can check the conversion. */
final class EchoHandler implements RequestHandlerInterface
{
    public function handle(ServerRequestInterface $request): ResponseInterface
    {
        $walk = static function (array $tree) use (&$walk): array {
            $out = [];
            foreach ($tree as $k => $v) {
                $out[$k] = $v instanceof UploadedFileInterface
                    ? ['name' => $v->getClientFilename(), 'size' => $v->getSize(), 'body' => (string) $v->getStream()]
                    : $walk($v);
            }
            return $out;
        };
        $attributes = $request->getAttributes();
        $context = $attributes['cairo.context'] ?? null;
        unset($attributes['cairo.context']);
        $body = json_encode([
            'kind' => 'psr15',
            'method' => $request->getMethod(),
            'uri' => (string) $request->getUri(),
            'protocol' => $request->getProtocolVersion(),
            'query' => $request->getQueryParams(),
            'header' => $request->getHeaderLine('X-Test'),
            'cookies' => $request->getCookieParams(),
            'parsed' => $request->getParsedBody(),
            'body' => (string) $request->getBody(),
            'files' => $walk($request->getUploadedFiles()),
            'attributes' => $attributes,
            'context' => $context instanceof Cairo\Context,
            'remote' => $request->getServerParams()['REMOTE_ADDR'] ?? null,
        ], JSON_THROW_ON_ERROR);
        return (new Psr7Response(201, ['Content-Type' => 'application/json', 'X-Handler' => ['psr15', 'echo']], $body));
    }
}

/** Stands in for a Symfony HttpKernelInterface (handle + terminate). */
final class MiniKernel
{
    /** @var list<string> */
    public static array $terminated = [];

    public function handle(SymfonyRequest $request): SymfonyResponse
    {
        if ($request->getPathInfo() === '/sf/stream') {
            return new StreamedResponse(static function (): void {
                echo "part 1\n";
                flush();   // the client has it now
                Cairo\sleep(0.3);
                echo "part 2\n";
            });
        }
        if ($request->getPathInfo() === '/sf/file') {
            return new BinaryFileResponse(__DIR__ . '/composer.json', 200, ['Content-Type' => 'application/json']);
        }
        if ($request->getPathInfo() === '/sf/terminated') {
            return new JsonResponse(self::$terminated);
        }
        $response = new JsonResponse([
            'kind' => 'symfony',
            'method' => $request->getMethod(),
            'path' => $request->getPathInfo(),
            'query' => $request->query->all(),
            'post' => $request->request->all(),
            'json' => $request->getContentTypeFormat() === 'json' ? $request->toArray() : null,
            'cookie' => $request->cookies->get('session'),
            'header' => $request->headers->get('X-Test'),
            'file' => ($f = $request->files->get('doc')) ? [$f->getClientOriginalName(), $f->isValid(), file_get_contents($f->getPathname())] : null,
            'id' => $request->attributes->get('id'),
            'secure' => $request->isSecure(),
            'client_ip' => $request->getClientIp(),
        ], 202);
        $response->headers->setCookie(Cookie::create('seen', 'yes'));
        return $response;
    }

    public function terminate(SymfonyRequest $request, SymfonyResponse $response): void
    {
        self::$terminated[] = $request->getPathInfo();
    }
}

$app = new App();
$http = $app->http(getenv('CAIRO_HTTP') ?: ':8080');

// A Cairo route next to the framework code.
$http->get('/cairo', fn () => 'cairo route');

// Extensions symfony/http-foundation needs that a minimal PHP build may lack.
$http->get('/missing-extensions', fn () => array_values(array_filter(['filter', 'json'], fn ($e) => !extension_loaded($e))));

// A PSR-15 handler on a route with a parameter, and the Symfony-style kernel
// on another (passed through Adapter::symfony() because MiniKernel does not
// implement HttpKernelInterface; a real kernel can be passed as is).
$http->route('POST', '/psr/{id}', new EchoHandler());
$http->get('/sf/{id}', Adapter::symfony(new MiniKernel()));
$http->post('/sf/{id}', Adapter::symfony(new MiniKernel()));
$http->get('/sf/stream', Adapter::symfony(new MiniKernel()));
$http->get('/sf/file', Adapter::symfony(new MiniKernel()));
$http->get('/sf/terminated', Adapter::symfony(new MiniKernel()));

// A PSR-7 body too large to be one answer goes out in pieces.
$http->get('/psr/large', Adapter::psr15(new class implements RequestHandlerInterface {
    public function handle(ServerRequestInterface $request): ResponseInterface
    {
        return new Psr7Response(200, ['Content-Type' => 'text/plain'], str_repeat("0123456789abcde\n", 3 << 16));
    }
}));

// Everything else goes to the PSR-15 handler.
$http->fallback(new EchoHandler());

return $app;
