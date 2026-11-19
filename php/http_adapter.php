<?php
/**
 * Cairo\Http\Adapter: PSR-15 handlers and Symfony or Laravel kernels as
 * Cairo handlers.
 */

declare(strict_types=1);

namespace Cairo\Http {

    /**
     * Runs framework code as Cairo handlers: PSR-15 request handlers (Slim,
     * Mezzio, any PSR-15 stack) through PSR-7, and Symfony or Laravel kernels
     * through HttpFoundation. Route handlers and $http->fallback() accept
     * those objects directly; these methods are for doing it by hand.
     */
    final class Adapter
    {
        /** @internal turns a framework object given as a handler into a Cairo handler */
        public static function wrap(object $handler): mixed
        {
            if ($handler instanceof \Psr\Http\Server\RequestHandlerInterface) {
                return self::psr15($handler);
            }
            if ($handler instanceof \Illuminate\Contracts\Http\Kernel) {
                return self::symfony($handler, 'Illuminate\Http\Request');
            }
            if ($handler instanceof \Symfony\Component\HttpKernel\HttpKernelInterface) {
                return self::symfony($handler);
            }
            return $handler;
        }

        /**
         * A Cairo handler running a PSR-15 handler. $factory makes the PSR-7
         * objects (a PSR-17 server request, stream and uploaded file factory in
         * one); by default nyholm/psr7's or guzzlehttp/psr7's, whichever is installed.
         */
        public static function psr15(object $handler, ?object $factory = null): \Closure
        {
            return static function (Request $req) use ($handler, &$factory): Response {
                $factory ??= self::psr17();
                return self::fromPsr7($handler->handle(self::toPsr7($req, $factory)));
            };
        }

        /**
         * A Cairo handler running a Symfony HttpKernelInterface, or a Laravel
         * HTTP Kernel with $requestClass 'Illuminate\Http\Request'.
         * terminate() runs after the response is sent.
         */
        public static function symfony(object $kernel, string $requestClass = 'Symfony\Component\HttpFoundation\Request'): \Closure
        {
            return static function (Request $req) use ($kernel, $requestClass): Response {
                $request = self::toSymfony($req, $requestClass);
                $response = $kernel->handle($request);
                if (method_exists($kernel, 'terminate')) {
                    \Cairo\Context::defer(static fn () => $kernel->terminate($request, $response));
                }
                return self::fromSymfony($response, $request);
            };
        }

        /** The PSR-17 factory of nyholm/psr7 or guzzlehttp/psr7. */
        public static function psr17(): object
        {
            foreach (['Nyholm\Psr7\Factory\Psr17Factory', 'GuzzleHttp\Psr7\HttpFactory'] as $class) {
                if (class_exists($class)) {
                    return new $class();
                }
            }
            throw new \LogicException('PSR-15 handlers need a PSR-7 implementation: composer require nyholm/psr7 (or guzzlehttp/psr7), or pass a PSR-17 factory');
        }

        /** A PSR-7 ServerRequestInterface for $req; route parameters become attributes. */
        public static function toPsr7(Request $req, object $factory): object
        {
            $event = $req->event();
            $g = \Cairo\Internal\Superglobals::build($event);
            $r = $factory->createServerRequest($req->method(), self::url($event), $g['server']);
            foreach ($event['headers'] ?? [] as $name => $values) {
                $r = $r->withHeader((string) $name, (array) $values);
            }
            $r = $r->withProtocolVersion(substr((string) ($event['proto'] ?? 'HTTP/1.1'), 5) ?: '1.1')
                ->withCookieParams($g['cookies'])
                ->withQueryParams($g['get'])
                ->withBody($factory->createStream($req->body()));
            if ($g['post'] !== [] || ($event['files'] ?? []) !== []) {
                $r = $r->withParsedBody($g['post']);
            }
            $files = [];
            foreach ($req->files() as $file) {
                $stream = $file->ok() ? $factory->createStreamFromFile($file->path) : $factory->createStream('');
                self::put($files, $file->field, $factory->createUploadedFile($stream, $file->size, $file->error, $file->name, $file->type));
            }
            $r = $r->withUploadedFiles($files);
            foreach ($event['params'] ?? [] as $name => $value) {
                $r = $r->withAttribute((string) $name, $value);
            }
            return $r->withAttribute('cairo.context', \Cairo\Context::current());
        }

        /** A Cairo Response from a PSR-7 ResponseInterface. */
        public static function fromPsr7(object $response): Response
        {
            $body = $response->getBody();
            if ($body->isSeekable()) {
                $body->rewind();
            }
            return new Response($response->getStatusCode(), $response->getHeaders(), $body->getContents());
        }

        /** A Symfony (or Laravel) Request for $req; route parameters become attributes. */
        public static function toSymfony(Request $req, string $class = 'Symfony\Component\HttpFoundation\Request'): object
        {
            $event = $req->event();
            $g = \Cairo\Internal\Superglobals::build($event);
            $body = ($event['files'] ?? []) !== [] ? '' : $req->body();
            $request = $class::create(self::url($event), $req->method(), $req->method() === 'GET' ? [] : $g['post'], $g['cookies'], $g['files'], $g['server'], $body);
            foreach ($event['params'] ?? [] as $name => $value) {
                $request->attributes->set((string) $name, $value);
            }
            $request->attributes->set('cairo.context', \Cairo\Context::current());
            return $request;
        }

        /** A Cairo Response from a Symfony (or Laravel) Response, streamed ones included. */
        public static function fromSymfony(object $response, ?object $request = null): Response
        {
            if ($request !== null) {
                $response->prepare($request);
            }
            ob_start();
            try {
                $response->sendContent();
            } finally {
                $body = (string) ob_get_clean();
            }
            $headers = $response->headers->allPreserveCaseWithoutCookies();
            foreach ($response->headers->getCookies() as $cookie) {
                $headers['Set-Cookie'][] = (string) $cookie;
            }
            return new Response($response->getStatusCode(), $headers, $body);
        }

        private static function url(array $event): string
        {
            $host = (string) ($event['host'] ?? '') ?: 'localhost';
            $query = (string) ($event['query'] ?? '');
            return (!empty($event['tls']) ? 'https' : 'http') . '://' . $host . $event['path'] . ($query !== '' ? '?' . $query : '');
        }

        /** Stores $value at form field $field ("doc", "docs[]", "a[b]") in $tree. */
        private static function put(array &$tree, string $field, mixed $value): void
        {
            $keys = [strtok($field, '[') ?: $field];
            if (preg_match_all('/\[([^\]]*)\]/', $field, $m)) {
                $keys = array_merge($keys, $m[1]);
            }
            $node = &$tree;
            foreach ($keys as $i => $key) {
                if ($i === count($keys) - 1) {
                    if ($key === '') {
                        $node[] = $value;
                    } else {
                        $node[$key] = $value;
                    }
                    return;
                }
                if ($key === '') {
                    $node[] = [];
                    $key = array_key_last($node);
                }
                $node[$key] ??= [];
                $node = &$node[$key];
            }
        }
    }
}
