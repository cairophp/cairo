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
        private string $scripts = '';
        private string $scriptsFront = 'index.php';
        /** @var list<array{dir:string,prefix:string}> */
        private array $sendfile = [];
        private string $metrics = '';
        private string $live = '';
        private string $ready = '';
        private float $readTimeout = 0;
        private float $writeTimeout = 0;
        /** @var list<\Cairo\Middleware> */
        private array $middleware = [];
        /** @var ?array<string, mixed> */
        private ?array $tls = null;
        private bool $h2c = false;
        private string $redirectHttp = '';
        private ?Mercure $mercure = null;

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
         * Middleware::timeout() does not apply to the route declared just
         * before this call: for an answer that lasts as long as its client
         * stays, as Response::events() does, or a long download.
         */
        public function noTimeout(): static
        {
            $last = array_key_last($this->routes);
            if ($last === null) {
                throw new \LogicException('noTimeout() applies to the route declared before it: declare a route first');
            }
            $this->routes[$last]['no_timeout'] = true;
            return $this;
        }

        /**
         * The handler of the route declared just before this call reads
         * the request's body in pieces, with Request::read(), while the
         * client sends it: an upload that is larger than memory, or a client
         * that goes on speaking while it is answered. body() is empty then,
         * a form is not parsed, and neither the 32 MiB limit nor the
         * server's read timeout applies: the handler says when it has
         * read enough.
         */
        public function streamBody(): static
        {
            $last = array_key_last($this->routes);
            if ($last === null) {
                throw new \LogicException('streamBody() applies to the route declared before it: declare a route first');
            }
            $this->routes[$last]['stream_body'] = true;
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

        /**
         * Serve the files under $dir at $prefix. Go serves them; no PHP
         * runs. The rules are publicDir()'s: PHP sources and hidden files
         * are never served, and paths cannot leave $dir, symlinks included.
         * A directory is served by its index.html, never listed.
         */
        public function static(string $prefix, string $dir): static
        {
            $this->static[] = ['prefix' => $prefix, 'dir' => $dir];
            return $this;
        }

        /**
         * Serve the files under $dir at their own paths, as a web server's
         * document root does (Laravel's public/), and hand every other request
         * to the routes. Go serves the files; no PHP runs. PHP sources and
         * hidden files (.env, .git) are never served, but for a top-level
         * .well-known (security.txt, ACME challenges), and paths cannot
         * leave $dir, symlinks included.
         */
        public function publicDir(string $dir): static
        {
            if (!is_dir($dir)) {
                throw new \InvalidArgumentException("publicDir: {$dir} is not a directory");
            }
            $this->publicDir = $dir;
            return $this;
        }

        /**
         * Serve the PHP scripts under $dir as a web server in front of
         * PHP-FPM does, for apps that are a set of scripts (WordPress):
         * /a/b.php runs b.php, /x.php/more runs x.php with PATH_INFO /more,
         * a directory runs its index.php, and a path that names nothing runs
         * $front, the front controller ($dir/index.php by default). The other files under $dir
         * are served by Go, as publicDir() serves them; the source of a
         * script never is, nor hidden files. Needs
         * App::concurrency('classic'): each request has a PHP request of its
         * own. The script sees the superglobals, php://input, echo and
         * header() as under PHP-FPM.
         */
        public function scripts(string $dir, string $front = 'index.php'): static
        {
            if ($front !== basename($front) || strcasecmp(pathinfo($front, PATHINFO_EXTENSION), 'php') !== 0) {
                throw new \InvalidArgumentException("scripts(): the front controller must be a .php file in \$dir, by its name");
            }
            $this->publicDir($dir);
            $this->scripts = $dir;
            $this->scriptsFront = $front;
            $this->route('*', '/', static fn (Request $req) => \Cairo\Internal\Scripts::run($req));
            $this->routes[array_key_last($this->routes)]['scripts'] = true;
            return $this;
        }

        /** @internal Every route of this server runs the script Go names (scripts()). */
        public function scriptsOnly(): bool
        {
            foreach ($this->routes as $route) {
                if (empty($route['scripts'])) {
                    return false;
                }
            }
            return $this->routes !== [];
        }

        /**
         * Let an answer name a file under $dir, which Go then sends in its
         * place: Response::file(), or the header X-Sendfile with the file's
         * path, as code written for Apache sets it. Symfony's and Laravel's
         * file responses do after BinaryFileResponse::trustXSendfileTypeHeader().
         * With $prefix, the header X-Accel-Redirect and a URL path under it
         * name the file too, as code written for nginx has it. Go sends
         * the file with ranges and If-Modified-Since while the PHP thread
         * goes on; a path cannot leave $dir, symlinks included.
         */
        public function sendfile(string $dir, string $prefix = ''): static
        {
            if (!is_dir($dir)) {
                throw new \InvalidArgumentException("sendfile: {$dir} is not a directory");
            }
            $this->sendfile[] = ['dir' => $dir, 'prefix' => $prefix];
            return $this;
        }

        /** Add Go-side middleware; see Cairo\Middleware. */
        public function use(\Cairo\Middleware $middleware): static
        {
            $this->middleware[] = $middleware;
            return $this;
        }

        /**
         * Serve the process's metrics at $path in Prometheus' text format,
         * and collect them: every HTTP request by method, route and status
         * with how long it took (cairo_http_request_duration_seconds), the
         * gRPC calls served (cairo_grpc_call_duration_seconds), the PHP
         * threads (cairo_php_*), the Go runtime and the process. Go answers;
         * no PHP runs. Without this call nothing is measured.
         *
         * The numbers describe the whole process, so declare the endpoint
         * on a server that only your monitoring reaches:
         *
         *     $app->http('127.0.0.1:9100')->metrics()->health();
         */
        public function metrics(string $path = '/metrics'): static
        {
            $this->metrics = $path;
            return $this;
        }

        /**
         * Serve a Mercure hub at /.well-known/mercure and return its
         * settings: see Mercure. A process has one hub.
         */
        public function mercure(): Mercure
        {
            return $this->mercure ??= new Mercure();
        }

        /**
         * Answer health probes without PHP. GET $live is 200 for as long as
         * the process serves: a liveness probe. GET $ready is 200 once the
         * PHP threads take requests and 503 from the moment the process is
         * told to stop: a readiness probe. `cairo serve --drain 5s` keeps
         * serving for that long after the signal, so a load balancer sees
         * the 503 and stops sending before connections close.
         */
        public function health(string $live = '/healthz', string $ready = '/readyz'): static
        {
            $this->live = $live;
            $this->ready = $ready;
            return $this;
        }

        public function timeouts(float $read = 0, float $write = 0): static
        {
            $this->readTimeout = $read;
            $this->writeTimeout = $write;
            return $this;
        }

        /**
         * Serve HTTPS with the certificate in $certFile (PEM: the
         * certificate, then its chain) and the private key in $keyFile.
         * Clients that ask for HTTP/2 get it. The files are read again when
         * they change, so a renewed certificate needs no restart.
         */
        public function tls(string $certFile, string $keyFile): static
        {
            $this->tls = ['cert' => $certFile, 'key' => $keyFile];
            return $this;
        }

        /**
         * Serve HTTPS with certificates that Let's Encrypt issues and renews
         * for $domains. Calling this accepts the terms of service of the
         * certificate authority.
         *
         * The authority checks each domain by connecting to it, so the
         * domains must point to this server and it must listen on port 443,
         * or have redirectHttp() on port 80. The first request for a domain
         * waits for its certificate. $cacheDir keeps the account key and the
         * certificates between restarts: it must survive them, and nobody
         * else should be able to read it. $email is where the authority
         * writes about problems with a certificate. $directory is the ACME
         * directory of another authority, or of Let's Encrypt's staging
         * service while you try things out.
         *
         * @param list<string> $domains host names, without wildcards
         */
        public function autoTls(array $domains, string $cacheDir, string $email = '', string $directory = ''): static
        {
            if ($domains === []) {
                throw new \InvalidArgumentException('autoTls: name at least one domain');
            }
            $this->tls = ['domains' => array_values($domains), 'cache_dir' => $cacheDir, 'email' => $email, 'directory' => $directory];
            return $this;
        }

        /**
         * Also accept HTTP/2 without TLS, from clients that start with it
         * (a load balancer or a proxy set to do so). HTTP/1.1 stays. Not
         * for a server with tls() or autoTls(), which has HTTP/2 already.
         */
        public function h2c(): static
        {
            $this->h2c = true;
            return $this;
        }

        /**
         * Also listen on $listen without TLS and send every request there
         * to the same address on this server (308). For a server with tls()
         * or autoTls().
         */
        public function redirectHttp(string $listen = ':80'): static
        {
            $this->redirectHttp = $listen;
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
                'scripts' => $this->scripts,
                'scripts_front' => $this->scriptsFront,
                'sendfile' => $this->sendfile,
                'middleware' => array_map(static fn ($m) => $m->spec(), $this->middleware),
                'tls' => $this->tls,
                'h2c' => $this->h2c,
                'redirect_http' => $this->redirectHttp,
                'metrics' => $this->metrics,
                'live' => $this->live,
                'ready' => $this->ready,
                'mercure' => $this->mercure?->spec(),
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

        /** The request's protocol: "HTTP/1.1" or "HTTP/2.0". */
        public function protocol(): string
        {
            return (string) ($this->event['proto'] ?? 'HTTP/1.1');
        }

        /**
         * Whether the request came to Cairo over TLS. Behind a proxy that
         * ends TLS itself this is false: read the header the proxy sets.
         */
        public function secure(): bool
        {
            return !empty($this->event['tls']);
        }

        public function body(): string
        {
            return (string) ($this->event['body'] ?? '');
        }

        /**
         * The next piece of the body, of $max bytes at most (1 MiB at most),
         * on a route with streamBody(); null once the client has sent
         * it all. Waits, its fiber only, for the client to send.
         *
         *     while (($piece = $req->read()) !== null) {
         *         fwrite($file, $piece);
         *     }
         *
         * @throws \Cairo\Context\Canceled the client has gone
         * @throws \RuntimeException the body cannot be read: its route has no streamBody()
         */
        public function read(int $max = 65536): ?string
        {
            return \Cairo\Internal\Go::call('http.body.read', ['max' => $max]);
        }

        /**
         * Sends 103 Early Hints ahead of the response, so that the browser
         * can start on what the page needs while the handler makes it:
         *
         *     $req->earlyHints(['Link' => '</app.css>; rel=preload; as=style']);
         *
         * @param array<string, string|list<string>> $headers
         */
        public function earlyHints(array $headers): void
        {
            $lists = [];
            foreach ($headers as $name => $value) {
                $lists[(string) $name] = array_map('strval', (array) $value);
            }
            \Cairo\Internal\Go::call('http.hints', ['status' => 103, 'headers' => $lists]);
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
}
