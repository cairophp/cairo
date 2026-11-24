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
        /** A PSR-7 body up to this size is one answer; a larger one is read and sent in pieces of PIECE. */
        private const WHOLE = 1 << 20;
        private const PIECE = 1 << 16;

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

        /**
         * The PSR-17 factories of the PSR-7 implementation that is installed:
         * nyholm/psr7, guzzlehttp/psr7, php-http/discovery's, laminas-diactoros
         * (Mezzio) or slim/psr7.
         */
        public static function psr17(): object
        {
            foreach (['Nyholm\Psr7\Factory\Psr17Factory', 'GuzzleHttp\Psr7\HttpFactory', 'Http\Discovery\Psr17Factory'] as $class) {
                if (class_exists($class)) {
                    return new $class();
                }
            }
            foreach (['Laminas\Diactoros\\', 'Slim\Psr7\Factory\\'] as $ns) {
                if (class_exists($ns . 'ServerRequestFactory')) {
                    return new \Cairo\Internal\Psr17($ns);
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

        /**
         * A Cairo Response from a PSR-7 ResponseInterface. A body of unknown
         * size, or a large one, is sent in pieces as it is read.
         */
        public static function fromPsr7(object $response): Response
        {
            $body = $response->getBody();
            if ($body->isSeekable()) {
                $body->rewind();
            }
            $size = $body->getSize();
            if ($size !== null && $size <= self::WHOLE) {
                return new Response($response->getStatusCode(), $response->getHeaders(), $body->getContents());
            }
            return Response::stream(static function (Stream $out) use ($body): void {
                while (!$body->eof()) {
                    $out->write($body->read(self::PIECE));
                }
            }, $response->getStatusCode(), $response->getHeaders());
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

        /**
         * A Cairo Response from a Symfony (or Laravel) Response. A streamed
         * response, and a file that no X-Sendfile header leaves to the
         * server, is sent right here, while its callback writes it: the
         * Response that comes back then only stands for it.
         */
        public static function fromSymfony(object $response, ?object $request = null): Response
        {
            if ($request !== null) {
                $response->prepare($request);
            }
            $headers = $response->headers->allPreserveCaseWithoutCookies();
            foreach ($response->headers->getCookies() as $cookie) {
                $headers['Set-Cookie'][] = (string) $cookie;
            }
            $status = $response->getStatusCode();
            $file = $response instanceof \Symfony\Component\HttpFoundation\BinaryFileResponse
                && !$response->headers->has('X-Sendfile') && !$response->headers->has('X-Accel-Redirect');
            if ($file || $response instanceof \Symfony\Component\HttpFoundation\StreamedResponse) {
                \Cairo\Internal\HttpOutput::send($status, $headers, static fn () => $response->sendContent());
                return new Response($status);
            }
            ob_start();
            try {
                $response->sendContent();
            } finally {
                $body = (string) ob_get_clean();
            }
            return new Response($status, $headers, $body);
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

namespace Cairo\Internal {

    /** The three PSR-17 factories that Adapter::toPsr7() needs, from an implementation that has them apart. */
    final class Psr17
    {
        private readonly object $requests;
        private readonly object $streams;
        private readonly object $files;

        public function __construct(string $ns)
        {
            $this->requests = new ($ns . 'ServerRequestFactory')();
            $this->streams = new ($ns . 'StreamFactory')();
            $this->files = new ($ns . 'UploadedFileFactory')();
        }

        /** @param array<string, mixed> $server */
        public function createServerRequest(string $method, mixed $uri, array $server = []): object
        {
            return $this->requests->createServerRequest($method, $uri, $server);
        }

        public function createStream(string $content = ''): object
        {
            return $this->streams->createStream($content);
        }

        public function createStreamFromFile(string $filename, string $mode = 'r'): object
        {
            return $this->streams->createStreamFromFile($filename, $mode);
        }

        public function createUploadedFile(object $stream, ?int $size = null, int $error = \UPLOAD_ERR_OK, ?string $clientFilename = null, ?string $clientMediaType = null): object
        {
            return $this->files->createUploadedFile($stream, $size, $error, $clientFilename, $clientMediaType);
        }
    }
}
