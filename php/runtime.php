<?php
/**
 * Cairo runtime: the PHP half of Cairo.
 *
 * Every PHP thread runs this file. It loads the user's server.php, which
 * returns a Cairo\App describing the servers. On the boot thread that
 * description is sent to Go (the declare phase). On worker threads the file
 * then runs the fiber scheduler: each request runs in its own Fiber, and a
 * fiber that waits on Go-backed I/O is suspended so the thread can run others.
 */

declare(strict_types=1);

namespace Cairo {

    use Cairo\Internal\Scheduler;

    final class App
    {
        /** @var list<Http\Server|Grpc\Server> */
        private array $servers = [];
        private int $threads = 0;
        private int $offloadThreads = 0;
        private int $maxRequests = 0;
        private int $maxMemory = 0;
        private int $hooks = Hook::ALL;
        private string $concurrency = 'fibers';
        /** @var list<object|callable> */
        private array $resets = [];
        /** @var list<mixed> handler id => handler */
        private array $handlers = [];

        /** Number of PHP threads: 'auto' (one per CPU core) or a number. */
        public function threads(int|string $threads): static
        {
            $this->threads = self::count($threads);
            return $this;
        }

        /** Number of PHP threads for Cairo\offload() jobs: 'auto' (one per CPU core) or a number. */
        public function offloadThreads(int|string $threads): static
        {
            $this->offloadThreads = self::count($threads);
            return $this;
        }

        private static function count(int|string $threads): int
        {
            if ($threads === 'auto') {
                return 0;
            }
            if (is_int($threads) && $threads > 0) {
                return $threads;
            }
            throw new \InvalidArgumentException("threads must be 'auto' or a positive integer");
        }

        /**
         * 'fibers' (the default): each PHP thread runs many requests at once.
         * 'isolated': each thread runs one request at a time, for apps that keep
         * request state in globals or singletons. Isolated requests also get
         * $_GET, $_POST, $_COOKIE, $_SERVER and $_REQUEST, and may echo their
         * output and call header() instead of returning a Response.
         */
        public function concurrency(string $mode): static
        {
            if ($mode !== 'fibers' && $mode !== 'isolated') {
                throw new \InvalidArgumentException("concurrency must be 'fibers' or 'isolated'");
            }
            $this->concurrency = $mode;
            return $this;
        }

        /** @internal */
        public function isolated(): bool
        {
            return $this->concurrency === 'isolated';
        }

        /**
         * Replace a request thread with a fresh one after it has served $n
         * requests (0, the default, never). The old thread finishes the
         * requests it holds first, so none fail. Use it to bound slow leaks.
         */
        public function maxRequests(int $n): static
        {
            if ($n < 0) {
                throw new \InvalidArgumentException('maxRequests must be 0 or more');
            }
            $this->maxRequests = $n;
            return $this;
        }

        /**
         * Replace a request thread with a fresh one once, after a request, its
         * PHP memory use (memory_get_usage()) is at least $limit: bytes, or a
         * string such as '256M' (0, the default, never).
         */
        public function maxMemory(int|string $limit): static
        {
            $bytes = is_int($limit) ? $limit : self::bytes($limit);
            if ($bytes < 0) {
                throw new \InvalidArgumentException('maxMemory must be 0 or more');
            }
            $this->maxMemory = $bytes;
            return $this;
        }

        private static function bytes(string $size): int
        {
            if (!preg_match('/^\s*(\d+)\s*([KMG]?)B?\s*$/i', $size, $m)) {
                throw new \InvalidArgumentException("invalid memory size '$size'; use bytes or a number with K, M or G");
            }
            return (int) $m[1] * match (strtoupper($m[2])) {
                'K' => 1024,
                'M' => 1024 ** 2,
                'G' => 1024 ** 3,
                default => 1,
            };
        }

        /** @return array{int, int} maxRequests and maxMemory in bytes; 0 means no limit */
        public function recycleLimits(): array
        {
            return [$this->maxRequests, $this->maxMemory];
        }

        /**
         * Things to reset after every request in isolated mode: objects with a
         * reset() method (Cairo\Resettable, Symfony's ResetInterface) or
         * callables. Use it for services that keep per-request state.
         */
        public function resetAfterRequest(object|callable ...$things): static
        {
            foreach ($things as $thing) {
                $this->resets[] = $thing;
            }
            return $this;
        }

        /** @internal */
        public function runResets(): void
        {
            foreach ($this->resets as $thing) {
                try {
                    if (is_object($thing) && !$thing instanceof \Closure && method_exists($thing, 'reset')) {
                        $thing->reset();
                    } else {
                        $thing();
                    }
                } catch (\Throwable $e) {
                    error_log('cairo: reset after request failed: ' . $e);
                }
            }
        }

        /**
         * Which blocking PHP functions suspend the fiber instead of blocking
         * the thread: Hook::ALL (the default), Hook::NONE, or Hook::* flags.
         */
        public function hooks(int $flags): static
        {
            $this->hooks = $flags;
            return $this;
        }

        /** @internal */
        public function hookFlags(): int
        {
            return $this->hooks;
        }

        public function http(string $listen): Http\Server
        {
            $server = new Http\Server($this, $listen);
            $this->servers[] = $server;
            return $server;
        }

        public function grpc(string $listen): Grpc\Server
        {
            $server = new Grpc\Server($this, $listen);
            $this->servers[] = $server;
            return $server;
        }

        /** @internal */
        public function register(mixed $handler): int
        {
            $this->handlers[] = $handler;
            return count($this->handlers) - 1;
        }

        /** @internal */
        public function handler(int $id): mixed
        {
            return $this->handlers[$id] ?? throw new \LogicException("unknown handler id $id");
        }

        /** @internal */
        public function spec(): array
        {
            $spec = ['threads' => $this->threads, 'offload_threads' => $this->offloadThreads, 'http' => [], 'grpc' => []];
            foreach ($this->servers as $server) {
                if ($server instanceof Http\Server) {
                    $spec['http'][] = $server->spec();
                } else {
                    $spec['grpc'][] = $server->spec();
                }
            }
            return $spec;
        }
    }

    /**
     * The Go context.Context of the current fiber, as PHP sees it.
     *
     * Every fiber is served by its own goroutine, which owns this context: the
     * request's root fiber by the goroutine that accepted the request, and each
     * async() fiber by a child goroutine with a child context.
     */
    final class Context
    {
        private static ?\WeakMap $byFiber = null;
        private static ?self $background = null;

        /** @var array<string, mixed> values set on this context */
        private array $values = [];
        /** @var list<callable> */
        private array $deferred = [];

        /** @internal */
        public function __construct(
            public readonly int $requestId,
            public readonly int $fiberId,
            private readonly ?float $deadline,
            private readonly ?self $parent = null,
        ) {
        }

        private bool $answered = false;

        /** @internal the request has been answered; its code is no longer stopped at the deadline */
        public function markAnswered(): void
        {
            $root = $this;
            while ($root->parent !== null) {
                $root = $root->parent;
            }
            $root->answered = true;
        }

        /** @internal */
        public function answered(): bool
        {
            $root = $this;
            while ($root->parent !== null) {
                $root = $root->parent;
            }
            return $root->answered;
        }

        /** @internal the context of a child fiber served by goroutine $fiberId */
        public function child(int $fiberId): self
        {
            return new self($this->requestId, $fiberId, $this->deadline, $this);
        }

        /**
         * Store a value for the current fiber and the async() tasks it starts.
         *
         * Values follow Go's context.WithValue: a child task sees its parent's
         * values, and a value set in a child stays in the child. Use this for
         * request state (user, tenant, trace id, a pooled connection) instead
         * of globals or static properties, which every fiber on the thread shares.
         */
        public static function set(string $key, mixed $value): void
        {
            self::current()->values[$key] = $value;
        }

        /** The value for $key in this fiber or its parents, or $default. */
        public static function get(string $key, mixed $default = null): mixed
        {
            for ($ctx = self::current(); $ctx !== null; $ctx = $ctx->parent) {
                if (array_key_exists($key, $ctx->values)) {
                    return $ctx->values[$key];
                }
            }
            return $default;
        }

        public static function has(string $key): bool
        {
            for ($ctx = self::current(); $ctx !== null; $ctx = $ctx->parent) {
                if (array_key_exists($key, $ctx->values)) {
                    return true;
                }
            }
            return false;
        }

        /** The value for $key; when there is none, store and return $make(). */
        public static function getOrSet(string $key, callable $make): mixed
        {
            if (self::has($key)) {
                return self::get($key);
            }
            $value = $make();
            self::set($key, $value);
            return $value;
        }

        /**
         * Run $fn when the current fiber ends (last deferred runs first). For a
         * request, that is after its response was sent.
         */
        public static function defer(callable $fn): void
        {
            self::current()->deferred[] = $fn;
        }

        /** @internal runs deferred callbacks and drops this fiber's values */
        public function finish(): void
        {
            while (($fn = array_pop($this->deferred)) !== null) {
                try {
                    $fn();
                } catch (\Throwable $e) {
                    error_log('cairo: deferred callback failed: ' . $e);
                }
            }
            $this->values = [];
        }

        public static function current(): self
        {
            $fiber = \Fiber::getCurrent();
            if ($fiber !== null && self::$byFiber !== null && isset(self::$byFiber[$fiber])) {
                return self::$byFiber[$fiber];
            }
            return self::$background ??= new self(0, 0, null);
        }

        /** @internal */
        public static function bind(\Fiber $fiber, self $context): void
        {
            self::$byFiber ??= new \WeakMap();
            self::$byFiber[$fiber] = $context;
        }

        public function deadline(): ?\DateTimeImmutable
        {
            if ($this->deadline === null) {
                return null;
            }
            return \DateTimeImmutable::createFromFormat('U.u', sprintf('%.6F', $this->deadline)) ?: null;
        }

        /** Seconds left before the deadline, or INF when there is none. */
        public function remaining(): float
        {
            return $this->deadline === null ? INF : max(0.0, $this->deadline - microtime(true));
        }

        public function err(): ?Context\ContextException
        {
            if ($this->deadline !== null && microtime(true) >= $this->deadline) {
                return new Context\DeadlineExceeded();
            }
            return null;
        }

        public function throwIfDone(): void
        {
            $err = $this->err();
            if ($err !== null) {
                throw $err;
            }
        }
    }

    /** A service that keeps per-request state; see App::resetAfterRequest(). */
    interface Resettable
    {
        public function reset(): void;
    }

    /** A unit of concurrent work started with async(). */
    final class Task
    {
        /** @internal */ public bool $done = false;
        /** @internal */ public mixed $result = null;
        /** @internal */ public ?\Throwable $error = null;
        /** @internal @var list<\Fiber> */ public array $waiters = [];
    }

    /**
     * Run $fn concurrently in its own fiber, under the current context.
     * It runs until its first suspension, then async() returns.
     */
    function async(callable $fn): Task
    {
        $task = new Task();
        $parent = Context::current();
        // Go starts the goroutine that will serve this fiber.
        $goroutine = \cairo_fiber_open($parent->fiberId);
        $context = $parent->child($goroutine);
        $fiber = new \Fiber(static function () use ($fn, $task, $goroutine, $context): void {
            try {
                $task->result = \cairo_guard($fn);
            } catch (\Throwable $e) {
                $task->error = $e;
            } finally {
                $context->finish();
                \cairo_fiber_close($goroutine);
            }
            $task->done = true;
            foreach ($task->waiters as $waiter) {
                Scheduler::instance()->ready($waiter);
            }
            $task->waiters = [];
        });
        Context::bind($fiber, $context);
        $fiber->start();
        return $task;
    }

    /** Wait for a task; rethrows its exception. */
    function await(Task $task): mixed
    {
        if (!$task->done) {
            $fiber = \Fiber::getCurrent() ?? throw new \LogicException('await() must be called inside a request (a fiber)');
            $task->waiters[] = $fiber;
            \Fiber::suspend();
        }
        if ($task->error !== null) {
            throw $task->error;
        }
        return $task->result;
    }

    /**
     * A task that finishes when all $tasks finish, with their results in the
     * same order (keys kept).
     * @param array<Task> $tasks
     */
    function all(array $tasks): Task
    {
        return async(static function () use ($tasks): array {
            $results = [];
            foreach ($tasks as $key => $task) {
                $results[$key] = await($task);
            }
            return $results;
        });
    }

    /**
     * Sleep without blocking the thread: the fiber suspends and its goroutine
     * waits. Throws Context\DeadlineExceeded or Context\Canceled if the
     * request's context ends first.
     */
    function sleep(float $seconds): void
    {
        if (\Fiber::getCurrent() === null) {
            usleep((int) ($seconds * 1e6));
            return;
        }
        Scheduler::instance()->op(['op' => 'sleep', 'seconds' => $seconds]);
    }

    /**
     * Run CPU-heavy work on the offload thread pool, so this request thread
     * keeps serving other fibers. The calling fiber suspends until it is done.
     *
     * Code cannot move between PHP threads, so $fn must be a function name or
     * [ClassName::class, 'staticMethod'] that server.php makes available, and
     * $args and the result must be serializable. Closures are not accepted.
     *
     * @param callable-string|array{0:class-string,1:string} $fn
     */
    function offload(string|array $fn, mixed ...$args): mixed
    {
        if (\Fiber::getCurrent() === null || !Scheduler::running()) {
            return $fn(...$args);   // already off the request threads
        }
        if (!is_callable($fn)) {
            throw new \InvalidArgumentException('offload() needs a function name or [Class::class, "staticMethod"]');
        }
        $payload = base64_encode(serialize([$fn, $args]));
        $result = Scheduler::instance()->op(['op' => 'offload', 'payload' => $payload]);
        $out = unserialize(base64_decode((string) $result));
        if (!($out['ok'] ?? false)) {
            throw new OffloadException(($out['class'] ?? 'Error') . ': ' . ($out['message'] ?? 'offloaded job failed'));
        }
        return $out['value'];
    }

    /**
     * A per-thread pool of connections (PDO, Redis clients, sockets), so each
     * fiber gets its own connection: a connection must never be used by two
     * fibers at once.
     *
     *     $db = new Pool(fn () => new PDO($dsn, $user, $pass), max: 10);
     *     $pdo = $db->get();                        // this fiber's, until it ends
     *     $db->with(fn (PDO $pdo) => $pdo->query('SELECT 1'));   // only for the callback
     *
     * Build pools in server.php: every PHP thread runs it, so every thread
     * gets its own pool. When all connections are in use, get() suspends the
     * fiber until one is returned.
     */
    final class Pool
    {
        /** @var list<mixed> */
        private array $idle = [];
        private int $open = 0;
        /** @var \WeakMap<Context, mixed> fiber context => its connection */
        private \WeakMap $held;
        /** @var \SplQueue<object{fiber: \Fiber, conn: mixed}> */
        private \SplQueue $waiters;
        /** @var \Closure(): mixed */
        private \Closure $factory;

        public function __construct(callable $factory, private readonly int $max = 16)
        {
            if ($max < 1) {
                throw new \InvalidArgumentException('a pool needs max >= 1');
            }
            $this->factory = \Closure::fromCallable($factory);
            $this->held = new \WeakMap();
            $this->waiters = new \SplQueue();
        }

        /**
         * The current fiber's connection: the same one on every call in this
         * fiber, returned to the pool when the fiber ends. A child task from
         * async() gets its own.
         */
        public function get(): mixed
        {
            $ctx = Context::current();
            if (isset($this->held[$ctx])) {
                return $this->held[$ctx];
            }
            $conn = $this->acquire();
            $this->held[$ctx] = $conn;
            Context::defer(fn () => $this->release($ctx));
            return $conn;
        }

        /** Borrow a connection for $fn only, then return it. */
        public function with(callable $fn): mixed
        {
            $conn = $this->acquire();
            try {
                return $fn($conn);
            } finally {
                $this->giveBack($conn);
            }
        }

        /** Give the current fiber's connection back before the fiber ends. */
        public function put(): void
        {
            $this->release(Context::current());
        }

        /** Drop the current fiber's connection (it broke); the pool may open a new one. */
        public function discard(): void
        {
            $ctx = Context::current();
            if (isset($this->held[$ctx])) {
                unset($this->held[$ctx]);
                $this->open--;
                $this->wakeOne();
            }
        }

        /** @return array{open: int, idle: int, waiting: int, max: int} */
        public function stats(): array
        {
            return ['open' => $this->open, 'idle' => count($this->idle), 'waiting' => count($this->waiters), 'max' => $this->max];
        }

        private function acquire(): mixed
        {
            if ($this->idle !== []) {
                return array_pop($this->idle);
            }
            if ($this->open < $this->max) {
                $this->open++;
                try {
                    return ($this->factory)();
                } catch (\Throwable $e) {
                    $this->open--;
                    throw $e;
                }
            }
            $fiber = \Fiber::getCurrent() ?? throw new \LogicException('the pool is exhausted and there is no fiber to wait in');
            $waiter = new class ($fiber) {
                public mixed $conn = null;
                public bool $fresh = false;
                public function __construct(public readonly \Fiber $fiber)
                {
                }
            };
            $this->waiters->enqueue($waiter);
            \Fiber::suspend();
            if ($waiter->fresh) {
                return $this->acquire();
            }
            return $waiter->conn;
        }

        private function release(Context $ctx): void
        {
            if (!isset($this->held[$ctx])) {
                return;
            }
            $conn = $this->held[$ctx];
            unset($this->held[$ctx]);
            $this->giveBack($conn);
        }

        private function giveBack(mixed $conn): void
        {
            if (!$this->waiters->isEmpty()) {
                $waiter = $this->waiters->dequeue();
                $waiter->conn = $conn;
                Scheduler::instance()->ready($waiter->fiber);
                return;
            }
            $this->idle[] = $conn;
        }

        /** A connection was dropped: let one waiter open a new one. */
        private function wakeOne(): void
        {
            if (!$this->waiters->isEmpty()) {
                $waiter = $this->waiters->dequeue();
                $waiter->fresh = true;
                Scheduler::instance()->ready($waiter->fiber);
            }
        }
    }

    /**
     * Middleware that Go runs before a request reaches PHP. Declare it on a
     * server, in order (the first one runs first):
     *
     *     $http->use(Middleware::requestId())
     *          ->use(Middleware::accessLog())
     *          ->use(Middleware::cors(origins: ['https://app.example']))
     *          ->use(Middleware::rateLimit(perSecond: 50, burst: 100))
     *          ->use(Middleware::timeout(30))
     *          ->use(Middleware::gzip());
     *
     * gRPC servers take requestId(), accessLog(), rateLimit() and timeout().
     */
    final class Middleware
    {
        private function __construct(public readonly string $name, public readonly array $options = [])
        {
        }

        /** Keeps a valid incoming X-Request-Id (gRPC: x-request-id) or sets a new one; it is echoed on the response. */
        public static function requestId(): self
        {
            return new self('request_id');
        }

        /** One log line per request: method, path, status, bytes, time, client, request id. */
        public static function accessLog(): self
        {
            return new self('access_log');
        }

        /**
         * Cross-origin requests. Preflights are answered in Go; PHP never sees them.
         * @param list<string> $origins allowed origins, or ['*']
         */
        public static function cors(
            array $origins,
            array $methods = [],
            array $headers = [],
            array $expose = [],
            bool $credentials = false,
            int $maxAge = 0,
        ): self {
            return new self('cors', [
                'origins' => array_values($origins), 'methods' => array_values($methods),
                'headers' => array_values($headers), 'expose' => array_values($expose),
                'credentials' => $credentials, 'max_age' => $maxAge,
            ]);
        }

        /**
         * A token bucket: $perSecond requests a second, bursts up to $burst.
         * Over the limit, HTTP answers 429 with Retry-After and gRPC answers
         * RESOURCE_EXHAUSTED. $by is 'ip' (a bucket per client) or 'global'.
         */
        public static function rateLimit(float $perSecond, int $burst = 0, string $by = 'ip'): self
        {
            return new self('rate_limit', ['per_second' => $perSecond, 'burst' => $burst, 'by' => $by]);
        }

        /** Rejects request bodies larger than $bytes with 413. HTTP only. */
        public static function maxBody(int $bytes): self
        {
            return new self('max_body', ['bytes' => $bytes]);
        }

        /**
         * Gives each request a deadline $seconds away (gRPC: the earlier of
         * this and the client's). Past it, PHP code is stopped with
         * Context\DeadlineExceeded even if it never waits on I/O, and the
         * client gets 504 or DEADLINE_EXCEEDED.
         */
        public static function timeout(float $seconds): self
        {
            return new self('timeout', ['seconds' => $seconds]);
        }

        /** Gzips responses of at least $minBytes for clients that accept it. HTTP only. */
        public static function gzip(int $minBytes = 1024): self
        {
            return new self('gzip', ['min_bytes' => $minBytes]);
        }

        /** @internal */
        public function spec(): array
        {
            return ['name' => $this->name, 'options' => (object) $this->options];
        }
    }

    /**
     * Runtime hooks: blocking PHP functions that suspend the current fiber
     * instead of blocking the whole thread. See App::hooks().
     */
    final class Hook
    {
        public const NONE = 0;
        /** sleep(), usleep(), time_nanosleep() */
        public const SLEEP = 1;
        /** reads and writes on tcp:// and unix:// streams: mysqlnd (PDO MySQL, mysqli), Redis clients, fsockopen() */
        public const SOCKETS = 2;
        /** curl_exec(): curl's socket waits suspend the fiber */
        public const CURL = 16;
        /** connects on tcp:// and unix:// streams, through Go's dialer (DNS included) */
        public const CONNECT = 4;
        /** gethostbyname(), gethostbynamel(), through Go's resolver */
        public const DNS = 8;
        /** PDO pgsql: Cairo's Go-backed driver runs each query on the fiber's goroutine */
        public const PGSQL = 32;
        public const ALL = self::SLEEP | self::SOCKETS | self::CONNECT | self::DNS | self::CURL | self::PGSQL;
    }

    /** An offloaded job threw; the message names the original exception class. */
    final class OffloadException extends \RuntimeException
    {
    }
}

namespace Cairo\Context {

    abstract class ContextException extends \RuntimeException
    {
    }

    final class DeadlineExceeded extends ContextException
    {
        public function __construct()
        {
            parent::__construct('context deadline exceeded');
        }
    }

    final class Canceled extends ContextException
    {
        public function __construct()
        {
            parent::__construct('context canceled');
        }
    }
}

namespace Cairo\Http {

    use Cairo\App;

    final class Server
    {
        /** @var list<array{id:int,method:string,path:string}> */
        private array $routes = [];
        /** @var list<array{prefix:string,dir:string}> */
        private array $static = [];
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
        public function route(string $method, string $path, callable|array $handler): static
        {
            $id = $this->app->register($handler);
            $this->routes[] = ['id' => $id, 'method' => strtoupper($method), 'path' => $path];
            return $this;
        }

        /** Serve files under $dir at $prefix. Go serves them; no PHP runs. */
        public function static(string $prefix, string $dir): static
        {
            $this->static[] = ['prefix' => $prefix, 'dir' => $dir];
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
                'middleware' => array_map(static fn ($m) => $m->spec(), $this->middleware),
            ];
        }
    }

    final class Request
    {
        /** @internal */
        private ?array $form = null;

        public function __construct(private readonly array $event)
        {
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
            parse_str($this->event['query'] ?? '', $query);
            $value = $query[$name] ?? null;
            return is_string($value) ? $value : null;
        }

        public function header(string $name): ?string
        {
            foreach ($this->event['headers'] ?? [] as $key => $values) {
                if (strcasecmp($key, $name) === 0) {
                    return $values[0] ?? null;
                }
            }
            return null;
        }

        public function body(): string
        {
            return base64_decode($this->event['body'] ?? '');
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

    /**
     * An HTTP client backed by Go's net/http: pooled keep-alive connections,
     * HTTP/2 over TLS, and proxies from HTTP_PROXY/HTTPS_PROXY/NO_PROXY.
     * Inside a request handler or async() task the fiber suspends while Go
     * sends the request, and the request's deadline cancels it; elsewhere
     * the thread waits.
     *
     *     $github = new Client(['base_uri' => 'https://api.github.com', 'timeout' => 5]);
     *     $repo = $github->get('/repos/php/php-src')->json();
     *
     * Options (to the constructor, or per request, which wins):
     *   base_uri         prefix for relative URLs
     *   headers          array<string, string|list<string>>
     *   query            array, appended to the URL
     *   body             string; or json (any value) or form (array)
     *   timeout          seconds for the whole exchange, 0 for none (default 30)
     *   follow_redirects bool, or the most redirects to follow (default 5)
     *   verify           bool, or a CA bundle path (default true)
     *   cert             client certificate PEM path, or [cert, key]
     *
     * Responses of any status are returned; transport failures throw
     * ClientException (ClientTimeoutException for the timeout option).
     */
    final class Client
    {
        /** @param array<string, mixed> $options */
        public function __construct(private readonly array $options = [])
        {
        }

        /** @param array<string, mixed> $options */
        public function get(string $url, array $options = []): ClientResponse
        {
            return $this->request('GET', $url, $options);
        }

        /** @param array<string, mixed> $options */
        public function post(string $url, array $options = []): ClientResponse
        {
            return $this->request('POST', $url, $options);
        }

        /** @param array<string, mixed> $options */
        public function put(string $url, array $options = []): ClientResponse
        {
            return $this->request('PUT', $url, $options);
        }

        /** @param array<string, mixed> $options */
        public function patch(string $url, array $options = []): ClientResponse
        {
            return $this->request('PATCH', $url, $options);
        }

        /** @param array<string, mixed> $options */
        public function delete(string $url, array $options = []): ClientResponse
        {
            return $this->request('DELETE', $url, $options);
        }

        /** @param array<string, mixed> $options */
        public function head(string $url, array $options = []): ClientResponse
        {
            return $this->request('HEAD', $url, $options);
        }

        /** @param array<string, mixed> $options */
        public function request(string $method, string $url, array $options = []): ClientResponse
        {
            $o = $options + $this->options;
            $headers = [];
            foreach ([$this->options['headers'] ?? [], $options['headers'] ?? []] as $set) {
                foreach ($set as $name => $value) {
                    $headers[strtolower((string) $name)] = [(string) $name, array_map('strval', (array) $value)];
                }
            }
            $setDefault = function (string $name, string $value) use (&$headers): void {
                $headers[strtolower($name)] ??= [$name, [$value]];
            };

            $body = '';
            if (array_key_exists('json', $o)) {
                $body = json_encode($o['json'], JSON_THROW_ON_ERROR | JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE);
                $setDefault('Content-Type', 'application/json');
            } elseif (isset($o['form'])) {
                $body = http_build_query($o['form'], '', '&', PHP_QUERY_RFC1738);
                $setDefault('Content-Type', 'application/x-www-form-urlencoded');
            } elseif (isset($o['body'])) {
                $body = (string) $o['body'];
            }

            if (isset($o['base_uri']) && !preg_match('~^[a-z][a-z0-9+.-]*://~i', $url)) {
                $url = rtrim((string) $o['base_uri'], '/') . '/' . ltrim($url, '/');
            }
            if (!empty($o['query'])) {
                $url .= (str_contains($url, '?') ? '&' : '?') . http_build_query($o['query'], '', '&', PHP_QUERY_RFC3986);
            }

            $follow = $o['follow_redirects'] ?? 5;
            $verify = $o['verify'] ?? true;
            $cert = (array) ($o['cert'] ?? []);
            $wire = [];
            foreach ($headers as [$name, $values]) {
                $wire[$name] = $values;
            }
            $result = \Cairo\Internal\Go::call('http', [
                'method' => strtoupper($method),
                'url' => $url,
                'headers' => (object) $wire,
                'body' => base64_encode($body),
                'timeout' => (float) ($o['timeout'] ?? 30),
                'redirects' => $follow === true ? 5 : (int) $follow,
                'tls' => [
                    'verify' => $verify !== false,
                    'cafile' => is_string($verify) ? $verify : '',
                    'cert' => (string) ($cert[0] ?? ''),
                    'key' => (string) ($cert[1] ?? ''),
                ],
            ]);
            if (!($result['ok'] ?? false)) {
                $message = (string) ($result['error'] ?? 'request failed');
                $kind = (string) ($result['kind'] ?? 'connect');
                throw $kind === 'timeout'
                    ? new ClientTimeoutException($message, $kind)
                    : new ClientException($message, $kind);
            }
            return new ClientResponse(
                (int) $result['status'],
                (string) $result['reason'],
                $result['headers'] ?: [],
                base64_decode((string) $result['body']),
                (string) $result['url'],
                (string) $result['protocol'],
            );
        }
    }

    final class ClientResponse
    {
        /** @param array<string, list<string>> $headers canonical names (Content-Type) */
        public function __construct(
            public readonly int $status,
            public readonly string $reason,
            public readonly array $headers,
            public readonly string $body,
            public readonly string $url,
            public readonly string $protocol,
        ) {
        }

        /** The first value of a header, by any case of its name. */
        public function header(string $name): ?string
        {
            foreach ($this->headers as $key => $values) {
                if (strcasecmp($key, $name) === 0) {
                    return $values[0] ?? null;
                }
            }
            return null;
        }

        public function ok(): bool
        {
            return $this->status >= 200 && $this->status < 300;
        }

        public function json(bool $assoc = true): mixed
        {
            return json_decode($this->body, $assoc, 512, JSON_THROW_ON_ERROR);
        }
    }

    /** A request that got no response: DNS, connect, TLS, redirects. */
    class ClientException extends \RuntimeException
    {
        /** $kind: connect, tls, timeout, redirects or request */
        public function __construct(string $message, public readonly string $kind = 'connect')
        {
            parent::__construct($message);
        }
    }

    /** The request's timeout option ran out. */
    final class ClientTimeoutException extends ClientException
    {
    }
}

namespace Cairo\Grpc {

    use Cairo\App;

    final class Server
    {
        /** @var list<string> */
        private array $protos = [];
        /** @var list<array{id:int,name:string}> */
        private array $services = [];
        private float $maxDuration = 0;
        /** @var list<\Cairo\Middleware> */
        private array $middleware = [];

        /** Add Go-side middleware: requestId(), accessLog(), rateLimit(); see Cairo\Middleware. */
        public function use(\Cairo\Middleware $middleware): static
        {
            $this->middleware[] = $middleware;
            return $this;
        }

        /** @internal */
        public function __construct(private readonly App $app, private readonly string $listen)
        {
        }

        /** A .proto file defining the services. Go compiles it at startup. */
        public function proto(string $file): static
        {
            $this->protos[] = $file;
            return $this;
        }

        /**
         * @param string $name fully qualified service name, e.g. 'helloworld.Greeter'
         * @param object|class-string $handler an object, or a class created once per thread.
         *        Each RPC method is called as $handler->Method(array $request, Context $ctx): array
         */
        public function service(string $name, object|string $handler): static
        {
            $id = $this->app->register($handler);
            $this->services[] = ['id' => $id, 'name' => $name];
            return $this;
        }

        /** Upper bound in seconds for any call, applied when the client sets a later or no deadline. */
        public function maxDuration(float $seconds): static
        {
            $this->maxDuration = $seconds;
            return $this;
        }

        /** @internal */
        public function spec(): array
        {
            return [
                'listen' => $this->listen,
                'protos' => $this->protos,
                'max_duration' => $this->maxDuration,
                'services' => $this->services,
                'middleware' => array_map(static fn ($m) => $m->spec(), $this->middleware),
            ];
        }
    }

    /** gRPC status codes. */
    final class Code
    {
        public const OK = 0;
        public const CANCELLED = 1;
        public const UNKNOWN = 2;
        public const INVALID_ARGUMENT = 3;
        public const DEADLINE_EXCEEDED = 4;
        public const NOT_FOUND = 5;
        public const ALREADY_EXISTS = 6;
        public const PERMISSION_DENIED = 7;
        public const RESOURCE_EXHAUSTED = 8;
        public const FAILED_PRECONDITION = 9;
        public const ABORTED = 10;
        public const OUT_OF_RANGE = 11;
        public const UNIMPLEMENTED = 12;
        public const INTERNAL = 13;
        public const UNAVAILABLE = 14;
        public const DATA_LOSS = 15;
        public const UNAUTHENTICATED = 16;
    }

    /** Throw from a handler to return a gRPC status to the client. */
    class Exception extends \RuntimeException
    {
        public function __construct(int $code, string $message = '', ?\Throwable $previous = null)
        {
            parent::__construct($message, $code, $previous);
        }
    }

    /**
     * A gRPC client over grpc-go, for unary calls. Messages are protojson
     * arrays (or generated message objects); Go encodes them with the
     * descriptors of the given .proto files. `cairo gen` also writes a typed
     * client per service (GreeterClient) that carries its descriptors.
     *
     *     $client = new Client('localhost:9000', ['protos' => [__DIR__ . '/greeter.proto']]);
     *     $reply = $client->call('/helloworld.Greeter/SayHello', ['name' => 'Cairo']);
     *
     * In a request handler or async() task only the fiber waits, and the
     * request's deadline becomes the call's deadline. Connections are shared
     * and reused across requests and threads.
     *
     * Options: protos (paths), descriptors (base64 FileDescriptorSet), tls
     * (true, or [cafile, cert, key, server_name, verify]), metadata
     * (sent with every call), timeout (seconds, default 30).
     */
    final class Client
    {
        /** @param array<string, mixed> $options */
        public function __construct(private readonly string $target, private readonly array $options = [])
        {
        }

        /**
         * Calls a unary method and returns the reply as a protojson array.
         * A status other than OK throws Cairo\Grpc\Exception with its code.
         *
         * @param array<string, mixed>|object $request an array, or a message with toArray()
         * @param array{metadata?: array<string, string|list<string>>, timeout?: float} $options
         * @return array<string, mixed>
         */
        public function call(string $method, array|object $request, array $options = []): array
        {
            return $this->invoke($method, $request, $options)->message;
        }

        /**
         * Like call(), with the reply's headers and trailers.
         *
         * @param array<string, mixed>|object $request
         * @param array{metadata?: array<string, string|list<string>>, timeout?: float} $options
         */
        public function invoke(string $method, array|object $request, array $options = []): Reply
        {
            $result = \Cairo\Internal\Go::call('grpc.call', $this->payload($method, $request, $options, 30));
            if (!($result['ok'] ?? false)) {
                throw new Exception((int) ($result['code'] ?? Code::UNKNOWN), (string) ($result['error'] ?? 'call failed'));
            }
            return new Reply($result['message'], $result['headers'] ?: [], $result['trailers'] ?: []);
        }

        /**
         * @param array<string, mixed>|object|null $request
         * @param array<string, mixed> $options
         * @return array<string, mixed>
         */
        private function payload(string $method, array|object|null $request, array $options, float $timeout): array
        {
            $tls = $this->options['tls'] ?? false;
            $tlsOpts = is_array($tls) ? $tls : [];
            $metadata = [];
            foreach ([$this->options['metadata'] ?? [], $options['metadata'] ?? []] as $set) {
                foreach ($set as $key => $value) {
                    $metadata[strtolower((string) $key)] = array_map('strval', (array) $value);
                }
            }
            return [
                'target' => $this->target,
                'method' => $method,
                'protos' => array_values((array) ($this->options['protos'] ?? [])),
                'descriptors' => (string) ($this->options['descriptors'] ?? ''),
                'message' => $request === null ? null : Stream::encode($request),
                'metadata' => (object) $metadata,
                'timeout' => (float) ($options['timeout'] ?? $this->options['timeout'] ?? $timeout),
                'tls' => [
                    'enabled' => $tls !== false,
                    'cafile' => (string) ($tlsOpts['cafile'] ?? ''),
                    'cert' => (string) ($tlsOpts['cert'] ?? ''),
                    'key' => (string) ($tlsOpts['key'] ?? ''),
                    'server_name' => (string) ($tlsOpts['server_name'] ?? ''),
                    'verify' => (bool) ($tlsOpts['verify'] ?? true),
                ],
            ];
        }

        /**
         * Opens a streaming call (client-streaming or bidirectional). $class
         * is the generated reply class recv() builds, or null for arrays.
         *
         * @param array{metadata?: array<string, string|list<string>>, timeout?: float} $options
         */
        public function stream(string $method, array $options = [], ?string $class = null): ClientStream
        {
            $r = Stream::call('grpc.stream.open', $this->payload($method, null, $options, 0));
            return new ClientStream((int) $r['id'], $class);
        }

        /**
         * Starts a server-streaming call with its one request; read the
         * replies with recv() or foreach.
         *
         * @param array<string, mixed>|object $request
         * @param array{metadata?: array<string, string|list<string>>, timeout?: float} $options
         */
        public function serverStream(string $method, array|object $request, array $options = [], ?string $class = null): ClientStream
        {
            $stream = $this->stream($method, $options, $class);
            $stream->send($request);
            $stream->closeSend();
            return $stream;
        }
    }

    /**
     * The stream of a streaming method being served. recv() returns the
     * client's next message (a generated object when the service implements
     * a generated interface, else a protojson array), or null once the client
     * has finished; send() sends one reply.
     *
     * @implements \IteratorAggregate<int, mixed>
     */
    final class ServerStream implements \IteratorAggregate
    {
        /** @internal */
        public function __construct(private readonly int $id, private readonly ?string $class = null)
        {
        }

        public function recv(): array|object|null
        {
            $r = Stream::call('grpc.server.recv', ['id' => $this->id]);
            if (!empty($r['eof'])) {
                return null;
            }
            return Stream::decode($r['message'], $this->class);
        }

        /** @param array<string, mixed>|object $message */
        public function send(array|object $message): void
        {
            Stream::call('grpc.server.send', ['id' => $this->id, 'message' => Stream::encode($message)]);
        }

        public function getIterator(): \Generator
        {
            while (($m = $this->recv()) !== null) {
                yield $m;
            }
        }
    }

    /**
     * A streaming call made with Client::stream() or serverStream(), or a
     * generated client's streaming method. send() and closeSend() for the
     * requests, recv() for the replies (null at the end; a failed call
     * throws Cairo\Grpc\Exception). Iterating reads the replies.
     *
     * @implements \IteratorAggregate<int, mixed>
     */
    final class ClientStream implements \IteratorAggregate
    {
        private bool $closed = false;
        /** @var array<string, list<string>> */
        private array $headers = [];
        /** @var array<string, list<string>> */
        private array $trailers = [];

        /** @internal */
        public function __construct(private readonly int $id, private readonly ?string $class = null)
        {
        }

        public function __destruct()
        {
            $this->close();
        }

        /** @param array<string, mixed>|object $message */
        public function send(array|object $message): void
        {
            Stream::call('grpc.stream.send', ['id' => $this->id, 'message' => Stream::encode($message)]);
        }

        /** No more requests: the server sees the end of the client's stream. */
        public function closeSend(): void
        {
            Stream::call('grpc.stream.closeSend', ['id' => $this->id]);
        }

        public function recv(): array|object|null
        {
            $r = Stream::call('grpc.stream.recv', ['id' => $this->id]);
            if (!empty($r['eof'])) {
                $this->headers = $r['headers'] ?: [];
                $this->trailers = $r['trailers'] ?: [];
                return null;
            }
            return Stream::decode($r['message'], $this->class);
        }

        /** For client-streaming methods: end the requests and read the one reply. */
        public function closeAndRecv(): array|object
        {
            $this->closeSend();
            $reply = $this->recv() ?? throw new Exception(Code::INTERNAL, 'the server sent no reply');
            $this->recv();   // the end of the stream, with the trailers
            return $reply;
        }

        /** Cancels the call if it is still running. */
        public function close(): void
        {
            if (!$this->closed) {
                $this->closed = true;
                \cairo_go_call('grpc.stream.close', json_encode(['id' => $this->id], JSON_THROW_ON_ERROR));
            }
        }

        /** @return array<string, list<string>> response headers, once the stream has ended */
        public function headers(): array
        {
            return $this->headers;
        }

        /** @return array<string, list<string>> trailers, once the stream has ended */
        public function trailers(): array
        {
            return $this->trailers;
        }

        public function getIterator(): \Generator
        {
            while (($m = $this->recv()) !== null) {
                yield $m;
            }
        }
    }

    /** @internal */
    final class Stream
    {
        /** @param array<string, mixed> $payload */
        public static function call(string $op, array $payload): array
        {
            $r = \Cairo\Internal\Go::call($op, $payload);
            if (!is_array($r) || !($r['ok'] ?? false)) {
                throw new Exception((int) ($r['code'] ?? Code::UNKNOWN), (string) ($r['error'] ?? 'stream failed'));
            }
            return $r;
        }

        public static function encode(array|object $message): mixed
        {
            if (is_object($message)) {
                $message = method_exists($message, 'toArray') ? $message->toArray() : get_object_vars($message);
            }
            return $message === [] ? new \stdClass() : $message;
        }

        public static function decode(mixed $message, ?string $class): array|object
        {
            $message = is_array($message) ? $message : [];
            return $class !== null ? $class::fromArray($message) : $message;
        }
    }

    final class Reply
    {
        /**
         * @param array<string, mixed> $message protojson
         * @param array<string, list<string>> $headers
         * @param array<string, list<string>> $trailers
         */
        public function __construct(
            public readonly array $message,
            public readonly array $headers,
            public readonly array $trailers,
        ) {
        }
    }
}

namespace Cairo\Net {

    use Cairo\Internal\Go;

    /**
     * Connect to $address over Go's net package. $network: tcp, tcp4, tcp6,
     * unix, udp, udp4, udp6, or tls (TCP + TLS client). In a request
     * handler or async() task only the fiber waits.
     *
     * Options: timeout (seconds for the connect, default 30), and for tls
     * the same "ssl" options as PHP stream contexts: verify_peer,
     * verify_peer_name, allow_self_signed, peer_name, cafile, capath,
     * local_cert, local_pk, passphrase, SNI_server_name.
     *
     * @param array<string, mixed> $options
     */
    function dial(string $network, string $address, array $options = []): Conn
    {
        $tls = $options;
        unset($tls['timeout']);
        $r = Net::call('net.dial', [
            'network' => $network,
            'address' => $address,
            'timeout' => (float) ($options['timeout'] ?? 30),
            'tls' => $network === 'tls' ? $tls + ['verify_peer' => true, 'verify_peer_name' => true] : null,
        ]);
        return new Conn((int) $r['id'], (string) $r['local'], (string) $r['remote']);
    }

    /**
     * Listen for connections. $network: tcp, tcp4, tcp6, unix, or tls (pass
     * cert, and key unless the cert file holds it). Port 0 picks a free one.
     *
     * @param array{cert?: string, key?: string} $options
     */
    function listen(string $network, string $address, array $options = []): Listener
    {
        $r = Net::call('net.listen', [
            'network' => $network,
            'address' => $address,
            'cert' => (string) ($options['cert'] ?? ''),
            'key' => (string) ($options['key'] ?? ''),
        ]);
        return new Listener((int) $r['id'], (string) $r['address']);
    }

    /** A datagram socket: udp, udp4, udp6 or unixgram. */
    function listenPacket(string $network, string $address): PacketConn
    {
        $r = Net::call('net.listenPacket', ['network' => $network, 'address' => $address]);
        return new PacketConn((int) $r['id'], (string) $r['address']);
    }

    /** @internal */
    final class Net
    {
        /** @param array<string, mixed> $payload */
        public static function call(string $op, array $payload): array
        {
            $r = Go::call($op, $payload);
            if (!is_array($r) || !($r['ok'] ?? false)) {
                $kind = (string) ($r['kind'] ?? 'io');
                $message = (string) ($r['error'] ?? 'network error');
                throw $kind === 'timeout' ? new TimeoutException($message, $kind) : new NetException($message, $kind);
            }
            return $r;
        }

        /** Closing never needs to wait, so it does not suspend (it may run in a destructor). */
        public static function close(int $id, string $mode = ''): void
        {
            \cairo_go_call('net.close', json_encode(['id' => $id, 'mode' => $mode], JSON_THROW_ON_ERROR));
        }
    }

    /**
     * A stream connection. Reads and writes wait for at most the timeout set
     * with setTimeout() (none by default), and for the request's deadline.
     */
    final class Conn
    {
        private bool $closed = false;
        private float $timeout = 0;

        /** @internal */
        public function __construct(
            private readonly int $id,
            public readonly string $localAddress,
            public readonly string $remoteAddress,
        ) {
        }

        public function __destruct()
        {
            $this->close();
        }

        /** Timeout in seconds for each read and write; null or 0 for none. */
        public function setTimeout(?float $seconds): static
        {
            $this->timeout = (float) $seconds;
            return $this;
        }

        /** Up to $max bytes, as soon as any arrive; '' at end of stream. */
        public function read(int $max = 65536): string
        {
            return $this->readMode('some', $max)[0];
        }

        /** Exactly $length bytes; throws NetException (kind "eof") if the peer closes first. */
        public function readExactly(int $length): string
        {
            [$data, $eof] = $this->readMode('exact', $length);
            if ($eof) {
                throw new NetException(sprintf('connection closed after %d of %d bytes', strlen($data), $length), 'eof');
            }
            return $data;
        }

        /**
         * Up to and including $eol (at most $max bytes); the rest of the
         * stream if it ends first, or null at end of stream.
         */
        public function readLine(string $eol = "\n", int $max = 65536): ?string
        {
            [$data, $eof] = $this->readMode('line', $max, $eol);
            return $eof && $data === '' ? null : $data;
        }

        /** Writes all of $data; returns its length. */
        public function write(string $data): int
        {
            $r = Net::call('net.write', ['id' => $this->id, 'data' => base64_encode($data), 'timeout' => $this->timeout]);
            return (int) $r['written'];
        }

        /** Shut down the writing side: the peer reads end of stream. */
        public function closeWrite(): void
        {
            Net::call('net.close', ['id' => $this->id, 'mode' => 'write']);
        }

        public function close(): void
        {
            if (!$this->closed) {
                $this->closed = true;
                Net::close($this->id);
            }
        }

        /** @return array{0: string, 1: bool} data and whether the stream ended */
        private function readMode(string $mode, int $max, string $eol = "\n"): array
        {
            $r = Net::call('net.read', ['id' => $this->id, 'mode' => $mode, 'max' => $max, 'eol' => $eol, 'timeout' => $this->timeout]);
            return [base64_decode((string) $r['data']), (bool) ($r['eof'] ?? false)];
        }
    }

    final class Listener
    {
        private bool $closed = false;

        /** @internal */
        public function __construct(private readonly int $id, public readonly string $address)
        {
        }

        public function __destruct()
        {
            $this->close();
        }

        /** The next connection; waits at most $timeout seconds (null: no limit). */
        public function accept(?float $timeout = null): Conn
        {
            $r = Net::call('net.accept', ['id' => $this->id, 'timeout' => (float) $timeout]);
            return new Conn((int) $r['id'], (string) $r['local'], (string) $r['remote']);
        }

        public function close(): void
        {
            if (!$this->closed) {
                $this->closed = true;
                Net::close($this->id);
            }
        }
    }

    final class PacketConn
    {
        private bool $closed = false;

        /** @internal */
        public function __construct(private readonly int $id, public readonly string $address)
        {
        }

        public function __destruct()
        {
            $this->close();
        }

        /** @return array{0: string, 1: string} the datagram and the sender's address */
        public function readFrom(int $max = 65536, ?float $timeout = null): array
        {
            $r = Net::call('net.readFrom', ['id' => $this->id, 'max' => $max, 'timeout' => (float) $timeout]);
            return [base64_decode((string) $r['data']), (string) $r['from']];
        }

        public function writeTo(string $data, string $address, ?float $timeout = null): int
        {
            $r = Net::call('net.writeTo', ['id' => $this->id, 'data' => base64_encode($data), 'address' => $address, 'timeout' => (float) $timeout]);
            return (int) $r['written'];
        }

        public function close(): void
        {
            if (!$this->closed) {
                $this->closed = true;
                Net::close($this->id);
            }
        }
    }

    /**
     * DNS through Go's resolver. Pass a server ("1.1.1.1:53") to ask it
     * instead of the system's resolver. Lookups of names that do not exist
     * throw NetException with kind "not_found".
     */
    final class Resolver
    {
        public function __construct(private readonly ?string $server = null, private readonly float $timeout = 10)
        {
        }

        /** @return list<string> IPv4 and IPv6 addresses */
        public function lookupHost(string $host): array
        {
            return $this->lookup('host', $host);
        }

        /** @return list<string> */
        public function lookupIPv4(string $host): array
        {
            return $this->lookup('ip4', $host);
        }

        /** @return list<string> */
        public function lookupIPv6(string $host): array
        {
            return $this->lookup('ip6', $host);
        }

        /** @return list<string> names for an address */
        public function lookupAddr(string $ip): array
        {
            return $this->lookup('addr', $ip);
        }

        public function lookupCNAME(string $host): string
        {
            return $this->lookup('cname', $host);
        }

        /** @return list<string> */
        public function lookupTXT(string $name): array
        {
            return $this->lookup('txt', $name);
        }

        /** @return list<array{host: string, pref: int}> by preference */
        public function lookupMX(string $name): array
        {
            return $this->lookup('mx', $name);
        }

        /** @return list<string> */
        public function lookupNS(string $name): array
        {
            return $this->lookup('ns', $name);
        }

        /**
         * _$service._$proto.$name; with '' for both, $name is looked up as is.
         *
         * @return list<array{target: string, port: int, priority: int, weight: int}>
         */
        public function lookupSRV(string $service, string $proto, string $name): array
        {
            return $this->lookup('srv', $name, ['service' => $service, 'proto' => $proto])['records'];
        }

        /** @param array<string, string> $extra */
        private function lookup(string $kind, string $name, array $extra = []): mixed
        {
            return Net::call('net.lookup', ['kind' => $kind, 'name' => $name, 'server' => (string) $this->server, 'timeout' => $this->timeout] + $extra)['result'];
        }
    }

    class NetException extends \RuntimeException
    {
        /** $kind: dial, tls, listen, timeout, closed, eof, io, not_found or lookup */
        public function __construct(string $message, public readonly string $kind = 'io')
        {
            parent::__construct($message);
        }
    }

    final class TimeoutException extends NetException
    {
    }
}

namespace Cairo\Internal {

    use Cairo\App;
    use Cairo\Context;
    use Cairo\Grpc;
    use Cairo\Http;

    /** One per PHP thread: runs every request in its own fiber. */
    final class Scheduler
    {
        private static ?self $instance = null;

        /** @var array<int, \Fiber> I/O operation id => waiting fiber */
        private array $io = [];
        private \SplQueue $ready;
        /** @var array<int, mixed> handler id => resolved handler */
        private array $resolved = [];

        /** Isolated mode: requests waiting for the one in progress to finish. */
        private \SplQueue $pending;
        private bool $busy = false;

        /** Thread recycling (App::maxRequests(), App::maxMemory()). */
        private int $active = 0;
        private int $served = 0;
        private bool $retireAsked = false;
        private bool $retiring = false;

        public function __construct(private readonly App $app)
        {
            $this->ready = new \SplQueue();
            $this->pending = new \SplQueue();
            self::$instance = $this;
        }

        public static function instance(): self
        {
            return self::$instance ?? throw new \LogicException('the Cairo scheduler is not running');
        }

        public static function running(): bool
        {
            return self::$instance !== null;
        }

        /** Ask the current fiber's goroutine to perform $op, and suspend until it is done. */
        public function op(array $op): mixed
        {
            $id = \cairo_fiber_op(Context::current()->fiberId, json_encode($op, JSON_THROW_ON_ERROR));
            if ($id < 0) {
                throw new \LogicException('Cairo I/O must run inside a request handler or an async() task');
            }
            return $this->waitIO($id);
        }

        /** Suspend the current fiber until Go reports I/O operation $id. */
        public function waitIO(int $id): mixed
        {
            $this->io[$id] = \Fiber::getCurrent() ?? throw new \LogicException('I/O must run inside a fiber');
            $event = \Fiber::suspend();
            if (isset($event['error'])) {
                throw match ($event['error']) {
                    'deadline_exceeded' => new Context\DeadlineExceeded(),
                    'canceled' => new Context\Canceled(),
                    default => new \RuntimeException((string) $event['error']),
                };
            }
            return $event['result'] ?? null;
        }

        public function ready(\Fiber $fiber): void
        {
            $this->ready->enqueue($fiber);
        }

        public function run(): void
        {
            while (true) {
                do {
                    while (!$this->ready->isEmpty()) {
                        $this->ready->dequeue()->resume();
                    }
                    // Isolated mode: start the next request once the thread is free.
                    if (!$this->busy && !$this->pending->isEmpty()) {
                        $this->start($this->pending->dequeue());
                        continue;
                    }
                    break;
                } while (true);
                if ($this->retiring && $this->active === 0 && $this->pending->isEmpty()) {
                    return;   // recycled: Go already runs a replacement thread
                }
                $event = json_decode(\cairo_next_event(), true, 512, JSON_THROW_ON_ERROR);
                switch ($event['type']) {
                    case 'io':
                        $fiber = $this->io[$event['io']] ?? null;
                        unset($this->io[$event['io']]);
                        $fiber?->resume($event);
                        break;
                    case 'http':
                    case 'grpc':
                        if ($this->app->isolated() && $this->busy) {
                            $this->pending->enqueue($event);
                        } else {
                            $this->start($event);
                        }
                        break;
                    case 'retire':
                        // Go queues no more requests here; finish the ones held.
                        $this->retiring = true;
                        break;
                    case 'shutdown':
                        return;
                }
            }
        }

        public function app(): App
        {
            return $this->app;
        }

        /** After a request: ask Go for a fresh thread once a recycle limit is reached. */
        private function checkRecycle(): void
        {
            if ($this->retireAsked) {
                return;
            }
            [$maxRequests, $maxMemory] = $this->app->recycleLimits();
            $reason = match (true) {
                $maxRequests > 0 && $this->served >= $maxRequests => 'max_requests',
                $maxMemory > 0 && memory_get_usage() >= $maxMemory => 'max_memory',
                default => null,
            };
            if ($reason !== null) {
                $this->retireAsked = true;
                \cairo_retire($reason);
            }
        }

        private function start(array $event): void
        {
            if (!$this->app->isolated()) {
                $this->spawn($event, $event['type'] === 'http' ? fn () => $this->http($event) : fn () => $this->grpc($event));
                return;
            }
            $this->busy = true;
            $this->spawn($event, function () use ($event): void {
                try {
                    if ($event['type'] === 'http') {
                        Superglobals::fill($event);
                        $this->http($event);
                    } else {
                        $this->grpc($event);
                    }
                } finally {
                    Superglobals::clear();
                    $this->app->runResets();
                    $this->busy = false;
                }
            });
        }

        private function spawn(array $event, \Closure $fn): void
        {
            $id = (int) $event['id'];   // the request's root fiber is served by the request goroutine
            $context = new Context($id, $id, isset($event['deadline']) ? (float) $event['deadline'] : null);
            $this->active++;
            $fiber = new \Fiber(function () use ($fn, $context): void {
                try {
                    $fn();
                } finally {
                    $context->finish();
                    $this->active--;
                    $this->served++;
                    $this->checkRecycle();
                }
            });
            Context::bind($fiber, $context);
            $fiber->start();
        }

        private function resolve(int $id): mixed
        {
            if (array_key_exists($id, $this->resolved)) {
                return $this->resolved[$id];
            }
            $handler = $this->app->handler($id);
            if (is_array($handler) && isset($handler[0]) && is_string($handler[0]) && class_exists($handler[0])) {
                $handler = [new $handler[0](), $handler[1]];
            } elseif (is_string($handler) && class_exists($handler)) {
                $handler = new $handler();
            }
            return $this->resolved[$id] = $handler;
        }

        private function http(array $event): void
        {
            $isolated = $this->app->isolated();
            Form::track($event['files'] ?? []);
            try {
                if ($isolated) {
                    // Classic PHP style: echo and header() become the response.
                    ob_start();
                    header_remove();
                    http_response_code(200);
                }
                try {
                    $handler = $this->resolve((int) $event['route']);
                    $response = \cairo_guard(static fn () => $handler(new Http\Request($event), Context::current()));
                } finally {
                    $output = $isolated ? (string) ob_get_clean() : '';
                }
                if ($isolated && $response === null) {
                    $headers = self::sentHeaders();
                    if (!isset(array_change_key_case($headers)['content-type'])) {
                        $headers['Content-Type'] = ['text/html; charset=UTF-8'];   // PHP's default
                    }
                    $response = new Http\Response((int) (http_response_code() ?: 200), $headers, $output);
                }
                if (!$response instanceof Http\Response) {
                    $response = is_string($response) ? Http\Response::text($response) : Http\Response::json($response);
                }
            } catch (Context\DeadlineExceeded) {
                $response = Http\Response::text("Gateway Timeout\n", 504);
            } catch (\Throwable $e) {
                error_log('cairo: ' . $e);
                $response = Http\Response::text("Internal Server Error\n", 500);
            }
            Context::current()->markAnswered();
            Form::untrack($event['files'] ?? []);   // Go removes what is left once we respond
            $headers = [];
            foreach ($response->headers as $name => $value) {
                $headers[$name] = is_array($value) ? array_values($value) : [$value];
            }
            \cairo_respond((int) $event['id'], json_encode([
                'status' => $response->status,
                'headers' => (object) $headers,
                'body' => base64_encode($response->body),
            ], JSON_THROW_ON_ERROR));
        }

        /** @return array<string, list<string>> the headers set with header() */
        private static function sentHeaders(): array
        {
            $out = [];
            foreach (headers_list() as $line) {
                [$name, $value] = array_pad(explode(':', $line, 2), 2, '');
                $out[trim($name)][] = ltrim($value);
            }
            header_remove();
            return $out;
        }

        /** @var array<string, ?string> "class::method" => request class, or null for arrays */
        private array $requestClasses = [];

        /**
         * The generated message class a handler takes as its first argument
         * (from cairo gen), or null when it takes the protojson array.
         */
        private function requestClass(object $service, string $method): ?string
        {
            $key = $service::class . '::' . $method;
            if (array_key_exists($key, $this->requestClasses)) {
                return $this->requestClasses[$key];
            }
            $class = null;
            $params = (new \ReflectionMethod($service, $method))->getParameters();
            $type = ($params[0] ?? null)?->getType();
            if ($type instanceof \ReflectionNamedType && !$type->isBuiltin()) {
                $name = $type->getName();
                if (method_exists($name, 'fromArray')) {
                    $class = $name;
                }
            }
            return $this->requestClasses[$key] = $class;
        }

        /**
         * A streaming method: server streaming handlers get (request, stream,
         * ctx) and send their replies; client-streaming and bidirectional ones
         * get (stream, ctx). A client-streaming handler returns its reply.
         */
        private function grpcStream(object $service, string $method, array $event, array $message): mixed
        {
            // Generated interfaces list each streaming method's message classes.
            $const = $service::class . '::STREAMING';
            [$in] = (defined($const) ? (constant($const)[$method] ?? null) : null) ?? [null, null];
            $stream = new Grpc\ServerStream((int) $event['stream'], $in);
            if (!empty($event['client_streaming'])) {
                $result = $service->{$method}($stream, Context::current());
            } else {
                $class = $this->requestClass($service, $method) ?? $in;
                $request = $class !== null ? $class::fromArray($message) : $message;
                $result = $service->{$method}($request, $stream, Context::current());
            }
            return empty($event['server_streaming']) ? $result : null;
        }

        private function grpc(array $event): void
        {
            try {
                $service = $this->resolve((int) $event['service']);
                $method = (string) $event['method'];
                if (!is_object($service) || !method_exists($service, $method)) {
                    throw new Grpc\Exception(Grpc\Code::UNIMPLEMENTED, "method $method is not implemented");
                }
                $message = $event['message'] ?? [];
                $result = \cairo_guard(function () use ($service, $method, $event, $message): mixed {
                    if (isset($event['stream'])) {
                        return $this->grpcStream($service, $method, $event, $message);
                    }
                    $class = $this->requestClass($service, $method);
                    $request = $class !== null ? $class::fromArray($message) : $message;
                    return $service->{$method}($request, Context::current());
                });
                if (is_object($result) && method_exists($result, 'toArray')) {
                    $result = $result->toArray();   // a class generated by cairo gen
                }
                $out = ['ok' => true, 'message' => ($result === [] || $result === null) ? new \stdClass() : $result];
            } catch (Grpc\Exception $e) {
                $out = ['ok' => false, 'code' => $e->getCode(), 'message' => $e->getMessage()];
            } catch (Context\DeadlineExceeded $e) {
                $out = ['ok' => false, 'code' => Grpc\Code::DEADLINE_EXCEEDED, 'message' => $e->getMessage()];
            } catch (Context\Canceled $e) {
                $out = ['ok' => false, 'code' => Grpc\Code::CANCELLED, 'message' => $e->getMessage()];
            } catch (\Throwable $e) {
                error_log('cairo: ' . $e);
                $out = ['ok' => false, 'code' => Grpc\Code::INTERNAL, 'message' => 'internal error'];
            }
            Context::current()->markAnswered();
            \cairo_respond((int) $event['id'], json_encode($out, JSON_THROW_ON_ERROR | JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE));
        }
    }

    /**
     * Runs a Go operation (see internal/sapi/goops.go): in a Cairo fiber the
     * fiber suspends while the fiber's goroutine runs it; elsewhere the
     * thread waits for Go.
     */
    final class Go
    {
        /** @param array<string, mixed> $payload */
        public static function call(string $op, array $payload): mixed
        {
            $json = json_encode($payload, JSON_THROW_ON_ERROR | JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE);
            if (Scheduler::running() && Context::current()->fiberId !== 0) {
                return Scheduler::instance()->op(['op' => $op, 'payload' => $json]);
            }
            $response = json_decode(\cairo_go_call($op, $json), true, 512, JSON_THROW_ON_ERROR);
            if (isset($response['error'])) {
                throw new \RuntimeException((string) $response['error']);
            }
            return $response['result'] ?? null;
        }
    }

    /** Fills and clears PHP's superglobals for isolated-mode requests. */
    final class Superglobals
    {
        public static function fill(array $event): void
        {
            $headers = $event['headers'] ?? [];
            parse_str((string) ($event['query'] ?? ''), $get);
            $cookies = [];
            $server = [
                'REQUEST_METHOD' => $event['method'],
                'REQUEST_URI' => $event['path'] . (($event['query'] ?? '') !== '' ? '?' . $event['query'] : ''),
                'SCRIPT_NAME' => '',
                'PATH_INFO' => $event['path'],
                'QUERY_STRING' => (string) ($event['query'] ?? ''),
                'SERVER_PROTOCOL' => (string) ($event['proto'] ?? 'HTTP/1.1'),
                'HTTPS' => !empty($event['tls']) ? 'on' : '',
                'REQUEST_TIME_FLOAT' => microtime(true),
                'REQUEST_TIME' => time(),
            ];
            [$host, $port] = self::split((string) ($event['remote'] ?? ''));
            $server['REMOTE_ADDR'] = $host;
            $server['REMOTE_PORT'] = $port;
            $server['SERVER_NAME'] = explode(':', (string) ($event['host'] ?? ''))[0];
            foreach ($headers as $name => $values) {
                $value = implode(', ', (array) $values);
                $key = strtoupper(str_replace('-', '_', (string) $name));
                if ($key === 'CONTENT_TYPE' || $key === 'CONTENT_LENGTH') {
                    $server[$key] = $value;
                } else {
                    $server['HTTP_' . $key] = $value;
                }
                if ($key === 'COOKIE') {
                    foreach (explode(';', $value) as $pair) {
                        [$k, $v] = array_pad(explode('=', trim($pair), 2), 2, '');
                        if ($k !== '') {
                            $cookies[urldecode($k)] = urldecode($v);
                        }
                    }
                }
            }
            $post = Form::fields($event);
            $_GET = $get;
            $_POST = $post;
            $_COOKIE = $cookies;
            $_SERVER = $server;
            $_REQUEST = array_merge($get, $post);
            $_FILES = Form::superglobal($event['files'] ?? []);
        }

        public static function clear(): void
        {
            $_GET = $_POST = $_COOKIE = $_REQUEST = $_FILES = [];
            $_SERVER = [];
        }

        /** @return array{string, string} */
        private static function split(string $addr): array
        {
            $i = strrpos($addr, ':');
            if ($i === false) {
                return [$addr, ''];
            }
            return [trim(substr($addr, 0, $i), '[]'), substr($addr, $i + 1)];
        }
    }

    /** Form bodies and uploads: $_POST/$_FILES shapes and move_uploaded_file(). */
    final class Form
    {
        /** @var array<string, true> temporary files of requests in flight on this thread */
        private static array $uploads = [];

        public static function fields(array $event): array
        {
            $fields = [];
            if (isset($event['form'])) {
                $pairs = [];
                foreach ($event['form'] as [$name, $value]) {
                    $pairs[] = urlencode($name) . '=' . urlencode($value);
                }
                parse_str(implode('&', $pairs), $fields);
            } else {
                foreach ($event['headers'] ?? [] as $name => $values) {
                    if (strcasecmp($name, 'Content-Type') === 0
                        && str_starts_with(strtolower((string) ($values[0] ?? '')), 'application/x-www-form-urlencoded')) {
                        parse_str(base64_decode((string) ($event['body'] ?? '')), $fields);
                    }
                }
            }
            return $fields;
        }

        /**
         * PHP's $_FILES layout: "doc" gives $_FILES['doc']['name'], "docs[]"
         * gives $_FILES['docs']['name'][0], "a[b]" gives $_FILES['a']['name']['b'].
         */
        public static function superglobal(array $files): array
        {
            $pairs = [];
            foreach ($files as $f) {
                $field = (string) $f['field'];
                $i = strpos($field, '[');
                $base = $i === false ? $field : substr($field, 0, $i);
                $rest = $i === false ? '' : substr($field, $i);
                if ($base === '') {
                    continue;
                }
                foreach (['name', 'full_path', 'type', 'tmp_name', 'error', 'size'] as $key) {
                    $pairs[] = urlencode($base) . '[' . $key . ']' . self::encodeBrackets($rest) . '=' . urlencode((string) $f[$key]);
                }
            }
            parse_str(implode('&', $pairs), $out);
            foreach ($out as &$entry) {
                foreach (['error', 'size'] as $key) {
                    if (is_array($entry[$key] ?? null)) {
                        array_walk_recursive($entry[$key], static function (&$v): void { $v = (int) $v; });
                    } elseif (isset($entry[$key])) {
                        $entry[$key] = (int) $entry[$key];
                    }
                }
            }
            unset($entry);
            return $out;
        }

        private static function encodeBrackets(string $rest): string
        {
            return preg_replace_callback('/\[([^\]]*)\]/', static fn (array $m): string => '[' . urlencode($m[1]) . ']', $rest) ?? '';
        }

        /** @param list<array{tmp_name: string}> $files */
        public static function track(array $files): void
        {
            foreach ($files as $f) {
                if ($f['tmp_name'] !== '') {
                    self::$uploads[$f['tmp_name']] = true;
                }
            }
        }

        /** @param list<array{tmp_name: string}> $files */
        public static function untrack(array $files): void
        {
            foreach ($files as $f) {
                unset(self::$uploads[$f['tmp_name']]);
            }
        }

        public static function isUpload(string $path): bool
        {
            return isset(self::$uploads[$path]);
        }

        public static function move(string $from, string $to): bool
        {
            if (!self::isUpload($from)) {
                return false;
            }
            if (!@rename($from, $to)) {
                if (!@copy($from, $to)) {
                    return false;
                }
                @unlink($from);
            }
            @chmod($to, 0666 & ~umask());
            unset(self::$uploads[$from]);
            return true;
        }
    }

    /**
     * Called from Cairo's C hooks when a hooked blocking function runs inside
     * a fiber. Returns false to let the original function block as usual.
     */
    final class Hooks
    {
        public static function sleep(float $seconds): bool
        {
            if (!self::on(\Cairo\Hook::SLEEP)) {
                return false;
            }
            Scheduler::instance()->op(['op' => 'sleep', 'seconds' => $seconds]);
            return true;
        }

        /**
         * Wait until socket $fd is readable (or writable). $timeout is the
         * stream's own timeout in seconds, -1 for none. Returns 1 when ready,
         * -1 on the stream timeout, 0 to let PHP block as usual.
         */
        public static function wait(int $fd, bool $write, float $timeout): int
        {
            if (!self::on(\Cairo\Hook::SOCKETS)) {
                return 0;
            }
            $result = Scheduler::instance()->op(['op' => 'wait', 'fd' => $fd, 'write' => $write, 'timeout' => $timeout]);
            return $result === 'timeout' ? -1 : 1;
        }

        /**
         * Wait for any of curl's sockets ($read readable, $write writable) or
         * $timeout seconds (-1: none). Returns 1 when done waiting, 0 to let
         * curl block as usual.
         *
         * @param list<int> $read
         * @param list<int> $write
         */
        public static function waitSet(array $read, array $write, float $timeout): int
        {
            if (!self::on(\Cairo\Hook::CURL)) {
                return 0;
            }
            Scheduler::instance()->op(['op' => 'waitset', 'read' => $read, 'write_fds' => $write, 'timeout' => $timeout]);
            return 1;
        }

        /**
         * Connect $address ("host:port", or a path for unix) through Go.
         * Returns the connected socket's descriptor, an error message, or
         * null to let PHP connect itself.
         */
        /** @param array<string, mixed> $tls the "ssl" context options, for $network "tls" */
        public static function connect(string $network, string $address, float $timeout, array $tls = []): int|string|null
        {
            if (!self::on(\Cairo\Hook::CONNECT)) {
                return null;
            }
            $op = ['op' => 'dial', 'network' => $network, 'address' => $address, 'timeout' => $timeout];
            if ($network === 'tls') {
                $op['tls'] = $tls;
            }
            $result = Scheduler::instance()->op($op);
            return isset($result['fd']) ? (int) $result['fd'] : (string) ($result['err'] ?? 'connect failed');
        }

        /**
         * stream_socket_enable_crypto() on socket $fd: Go handshakes as a TLS
         * client for $host. Returns the descriptor to put in place of $fd, an
         * error message, or null to let the thread wait for Go instead.
         *
         * @param array<string, mixed> $tls the "ssl" context options
         */
        public static function startTLS(int $fd, string $host, float $timeout, array $tls): int|string|null
        {
            if (!self::on(\Cairo\Hook::CONNECT)) {
                return null;
            }
            $result = Scheduler::instance()->op(['op' => 'starttls', 'fd' => $fd, 'host' => $host, 'timeout' => $timeout, 'tls' => $tls]);
            return isset($result['fd']) ? (int) $result['fd'] : (string) ($result['err'] ?? 'TLS handshake failed');
        }

        public static function stopTLS(int $fd, float $timeout): int|string|null
        {
            if (!self::on(\Cairo\Hook::CONNECT)) {
                return null;
            }
            $result = Scheduler::instance()->op(['op' => 'stoptls', 'fd' => $fd, 'timeout' => $timeout]);
            return isset($result['fd']) ? (int) $result['fd'] : (string) ($result['err'] ?? 'TLS shutdown failed');
        }

        public static function gethostbyname(mixed $hostname = null): ?string
        {
            if (!is_string($hostname) || !self::on(\Cairo\Hook::DNS)) {
                return null;
            }
            $ips = self::lookup($hostname);
            return $ips[0] ?? $hostname;   // like PHP: the name itself on failure
        }

        public static function gethostbynamel(mixed $hostname = null): array|false|null
        {
            if (!is_string($hostname) || !self::on(\Cairo\Hook::DNS)) {
                return null;
            }
            $ips = self::lookup($hostname);
            return $ips === [] ? false : $ips;
        }

        /**
         * One request of Cairo's PDO pgsql driver (JSON in, JSON out), run on
         * the fiber's goroutine. Null lets the driver wait for Go instead.
         */
        public static function pg(string $request): ?string
        {
            if (!self::on(\Cairo\Hook::PGSQL)) {
                return null;
            }
            $result = Scheduler::instance()->op(['op' => 'pg', 'payload' => $request]);
            return json_encode($result, JSON_THROW_ON_ERROR);
        }

        /** is_uploaded_file(): true for this thread's Cairo uploads. */
        public static function isUploadedFile(mixed $path = null): ?bool
        {
            return is_string($path) && Form::isUpload($path) ? true : null;
        }

        /** move_uploaded_file() for Cairo uploads; null leaves others to PHP. */
        public static function moveUploadedFile(mixed $from = null, mixed $to = null): ?bool
        {
            if (!is_string($from) || !is_string($to) || !Form::isUpload($from)) {
                return null;
            }
            return Form::move($from, $to);
        }

        /** @return list<string> */
        private static function lookup(string $host): array
        {
            if (ip2long($host) !== false) {
                return [$host];
            }
            return (array) Scheduler::instance()->op(['op' => 'lookup', 'host' => $host]);
        }

        /** Hooks run only in fibers Cairo started on a request thread. */
        private static function on(int $hook): bool
        {
            return Scheduler::running()
                && (Scheduler::instance()->app()->hookFlags() & $hook) !== 0
                && Context::current()->fiberId !== 0;
        }
    }

    /** Runs Cairo\offload() jobs, one at a time, on an offload thread. */
    final class OffloadWorker
    {
        public function run(): void
        {
            while (true) {
                $event = json_decode(\cairo_next_event(), true, 512, JSON_THROW_ON_ERROR);
                if ($event['type'] === 'shutdown') {
                    return;
                }
                if ($event['type'] !== 'offload') {
                    continue;
                }
                try {
                    [$fn, $args] = unserialize(base64_decode($event['payload']));
                    $out = ['ok' => true, 'value' => $fn(...$args)];
                } catch (\Throwable $e) {
                    $out = ['ok' => false, 'class' => $e::class, 'message' => $e->getMessage()];
                }
                \cairo_respond((int) $event['id'], json_encode(['payload' => base64_encode(serialize($out))], JSON_THROW_ON_ERROR));
            }
        }
    }

    /**
     * Called inside the running fiber when Go interrupts the thread because a
     * request's deadline passed (see cairo_on_interrupt in cairo.c): whether
     * the fiber's code, in $file, is to be stopped.
     */
    function interrupt(?string $file): bool
    {
        if ($file === __FILE__) {
            return false;   // never inside Cairo's runtime (it answers the request); Go pokes again
        }
        $context = \Cairo\Context::current();
        return !$context->answered() && $context->err() instanceof \Cairo\Context\DeadlineExceeded;
    }

    function main(): void
    {
        $info = json_decode(\cairo_info(), true, 512, JSON_THROW_ON_ERROR);
        $app = (static fn (string $file): mixed => require $file)($info['server']);
        if (!$app instanceof App) {
            throw new \RuntimeException($info['server'] . ' must return a Cairo\App');
        }
        \cairo_spec(json_encode($app->spec(), JSON_THROW_ON_ERROR | JSON_UNESCAPED_SLASHES));
        if ($info['mode'] === 'serve') {
            (new Scheduler($app))->run();
        } elseif ($info['mode'] === 'offload') {
            (new OffloadWorker())->run();
        }
    }
}

namespace {
    \Cairo\Internal\main();
}
