<?php
/**
 * The HTTP server on the PHP side: Cairo\Http\Server declares it, and a
 * handler gets a Request and returns a Response.
 */

declare(strict_types=1);

namespace Cairo\Http {

    use Cairo\App;

    final class Server
    {
        /** @var list<array{id:int,method:string,path:string,cache?:array{ttl:float,stale_while_revalidate:float,vary:list<string>,tags:list<string>}}> */
        private array $routes = [];
        /** @var list<array{prefix:string,dir:string}> */
        private array $static = [];
        private string $publicDir = '';
        private float $readTimeout = 0;
        private float $writeTimeout = 0;
        /** @var list<\Cairo\Middleware> */
        private array $middleware = [];

        /** @internal */
        public function __construct(private readonly App $app, private readonly string $listen)
        {
        }

        public function get(string $path, callable|array $handler): static
        {
            return $this->route('GET', $path, $handler);
        }

        public function post(string $path, callable|array $handler): static
        {
            return $this->route('POST', $path, $handler);
        }

        public function put(string $path, callable|array $handler): static
        {
            return $this->route('PUT', $path, $handler);
        }

        public function patch(string $path, callable|array $handler): static
        {
            return $this->route('PATCH', $path, $handler);
        }

        public function delete(string $path, callable|array $handler): static
        {
            return $this->route('DELETE', $path, $handler);
        }

        /**
         * @param callable|array{0:class-string,1:string} $handler a callable, or
         *        [Controller::class, 'method'] (the controller is created once per thread)
         */
        public function route(string $method, string $path, callable|array|object $handler): static
        {
            $id = $this->app->register($handler, strtoupper($method) . ' ' . $path);
            $this->routes[] = ['id' => $id, 'method' => strtoupper($method), 'path' => $path];
            return $this;
        }

        /**
         * Let Go answer repeated GET requests to the route declared just
         * before this call, without running PHP.
         *
         * A response stays fresh for $ttl seconds. After that, for
         * $staleWhileRevalidate more seconds, Go keeps serving it and rebuilds
         * it with one request in the background. Identical requests that
         * arrive while PHP builds a response wait for that one.
         *
         * The key is the route, host, path, raw query string and the $vary
         * request headers. Only 200 responses are stored, and never one with
         * Set-Cookie or Cache-Control private, no-store or no-cache. A request
         * with Authorization or Cookie skips the cache unless $vary names
         * that header.
         *
         * $tags name the route's responses for Cairo\Cache::purge(), which
         * drops them when the data behind them changes.
         *
         * @param list<string> $vary request headers that change the response
         * @param list<string> $tags names to purge the route's responses by
         */
        public function cache(float $ttl, float $staleWhileRevalidate = 0, array $vary = [], array $tags = []): static
        {
            $last = array_key_last($this->routes);
            if ($last === null) {
                throw new \LogicException('cache() applies to the route declared before it: declare a route first');
            }
            $this->routes[$last]['cache'] = [
                'ttl' => $ttl,
                'stale_while_revalidate' => $staleWhileRevalidate,
                'vary' => array_values($vary),
                'tags' => array_values($tags),
            ];
            return $this;
        }

        /**
         * Handle every request no other route or static path matches, with
         * any method: a Cairo handler, a PSR-15 RequestHandlerInterface, a
         * Symfony HttpKernelInterface or a Laravel HTTP Kernel (see
         * Cairo\Http\Adapter). This is how a whole framework app runs on Cairo.
         * A "GET /" route also matches every GET path, so leave it out then.
         */
        public function fallback(callable|array|object $handler): static
        {
            return $this->route('*', '/', $handler);
        }

        /** Serve files under $dir at $prefix. Go serves them; no PHP runs. */
        public function static(string $prefix, string $dir): static
        {
            $this->static[] = ['prefix' => $prefix, 'dir' => $dir];
            return $this;
        }

        /**
         * Serve the files under $dir at their own paths, as a web server's
         * document root does (Laravel's public/), and hand every other request
         * to the routes. Go serves the files; no PHP runs. PHP sources and
         * hidden files (.env, .git) are never served, and paths cannot leave
         * $dir, symlinks included.
         */
        public function publicDir(string $dir): static
        {
            if (!is_dir($dir)) {
                throw new \InvalidArgumentException("publicDir: {$dir} is not a directory");
            }
            $this->publicDir = $dir;
            return $this;
        }

        /** Add Go-side middleware; see Cairo\Middleware. */
        public function use(\Cairo\Middleware $middleware): static
        {
            $this->middleware[] = $middleware;
            return $this;
        }

        public function timeouts(float $read = 0, float $write = 0): static
        {
            $this->readTimeout = $read;
            $this->writeTimeout = $write;
            return $this;
        }

        /** @internal */
        public function spec(): array
        {
            return [
                'listen' => $this->listen,
                'read_timeout' => $this->readTimeout,
                'write_timeout' => $this->writeTimeout,
                'routes' => $this->routes,
                'static' => $this->static,
                'public_dir' => $this->publicDir,
                'middleware' => array_map(static fn ($m) => $m->spec(), $this->middleware),
            ];
        }
    }

    final class Request
    {
        /** @internal */
        private ?array $form = null;

        /** @var ?array<int|string, mixed> the query string, parsed at its first use */
        private ?array $query = null;

        public function __construct(private readonly array $event)
        {
        }

        /** @internal the request as Go sent it */
        public function event(): array
        {
            return $this->event;
        }

        public function method(): string
        {
            return $this->event['method'];
        }

        public function path(): string
        {
            return $this->event['path'];
        }

        public function param(string $name): ?string
        {
            return $this->event['params'][$name] ?? null;
        }

        public function query(string $name): ?string
        {
            if ($this->query === null) {
                parse_str($this->event['query'] ?? '', $query);
                $this->query = $query;
            }
            $value = $this->query[$name] ?? null;
            return is_string($value) ? $value : null;
        }

        public function header(string $name): ?string
        {
            $headers = $this->event['headers'] ?? [];
            // Go sends the names in their canonical form ("Content-Type"),
            // which is how most code asks for them.
            if (isset($headers[$name])) {
                return $headers[$name][0] ?? null;
            }
            foreach ($headers as $key => $values) {
                if (strcasecmp((string) $key, $name) === 0) {
                    return $values[0] ?? null;
                }
            }
            return null;
        }

        public function body(): string
        {
            return (string) ($this->event['body'] ?? '');
        }

        public function json(): mixed
        {
            return json_decode($this->body(), true, 512, JSON_THROW_ON_ERROR);
        }

        /**
         * The form fields of a urlencoded or multipart/form-data body, nested
         * by PHP's bracket rules ("tags[]", "a[b]"), like $_POST.
         */
        public function form(): array
        {
            return $this->form ??= \Cairo\Internal\Form::fields($this->event);
        }

        /** The first uploaded file sent as $field (or "$field[]"), if any. */
        public function file(string $field): ?UploadedFile
        {
            foreach ($this->files() as $file) {
                if ($file->field === $field || $file->field === $field . '[]') {
                    return $file;
                }
            }
            return null;
        }

        /** @return list<UploadedFile> every file part, in request order */
        public function files(): array
        {
            return array_map(static fn (array $f): UploadedFile => new UploadedFile($f), $this->event['files'] ?? []);
        }
    }

    /**
     * A file from a multipart/form-data request. Its temporary file is removed
     * after the request unless moved with moveTo() (or move_uploaded_file()).
     */
    final class UploadedFile
    {
        public readonly string $field;
        /** The client's file name, without directories. */
        public readonly string $name;
        public readonly string $type;
        public readonly int $size;
        public readonly string $path;
        /** One of PHP's UPLOAD_ERR_* codes. */
        public readonly int $error;

        /** @internal */
        public function __construct(array $f)
        {
            $this->field = (string) $f['field'];
            $this->name = (string) $f['name'];
            $this->type = (string) $f['type'];
            $this->size = (int) $f['size'];
            $this->path = (string) $f['tmp_name'];
            $this->error = (int) $f['error'];
        }

        public function ok(): bool
        {
            return $this->error === UPLOAD_ERR_OK;
        }

        public function contents(): string
        {
            return $this->ok() ? (string) file_get_contents($this->path) : '';
        }

        /** Moves the file to $target; throws when it cannot. */
        public function moveTo(string $target): void
        {
            if (!$this->ok() || !\Cairo\Internal\Form::move($this->path, $target)) {
                throw new \RuntimeException("cannot move upload {$this->name} to {$target}");
            }
        }
    }

    final class Response
    {
        /** @param array<string, string|list<string>> $headers */
        public function __construct(
            public readonly int $status = 200,
            public readonly array $headers = [],
            public readonly string $body = '',
        ) {
        }

        public static function text(string $body, int $status = 200): self
        {
            return new self($status, ['Content-Type' => 'text/plain; charset=utf-8'], $body);
        }

        public static function html(string $body, int $status = 200): self
        {
            return new self($status, ['Content-Type' => 'text/html; charset=utf-8'], $body);
        }

        public static function json(mixed $data, int $status = 200): self
        {
            $body = json_encode($data, JSON_THROW_ON_ERROR | JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE);
            return new self($status, ['Content-Type' => 'application/json'], $body);
        }
    }
}
