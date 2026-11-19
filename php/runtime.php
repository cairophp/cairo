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
        private float $detectBlocking = 0.0;
        private float $slowRequests = 0.0;
        private float $heavyRoutes = 0.0;
        private int $maxInFlight = 0;
        private int $repeatedQueries = 0;
        private int $hooks = Hook::ALL;
        private string $concurrency = 'fibers';
        /** @var list<object|callable> */
        private array $resets = [];
        /** @var list<mixed> handler id => handler */
        private array $handlers = [];
        /** @var list<string> handler id => route ("GET /users/{id}", "helloworld.Greeter") */
        private array $routes = [];

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
         * Development aid: log, with its stack, any request that keeps its
         * PHP thread busy for more than $seconds without waiting on Cairo (a
         * CPU loop, or a blocking call Cairo does not hook), since every other
         * request on that thread waits meanwhile. 0 (the default) turns it off.
         * Isolated mode, where blocking is expected, is not reported.
         */
        public function detectBlocking(float $seconds = 0.1): static
        {
            if ($seconds < 0) {
                throw new \InvalidArgumentException('detectBlocking must be 0 or more');
            }
            $this->detectBlocking = $seconds;
            return $this;
        }

        /**
         * Log a warning when one request runs the same statement $times
         * times or more: the N+1 pattern, a query in a loop where one query
         * with a join or IN (...) would do.
         *
         *     level=WARN msg="repeated query" route="GET /orders" count=50 query="SELECT * FROM items WHERE order_id = ?"
         *
         * It counts every statement run through PDO (any driver) and
         * Cairo\Sql. Statements with the same shape count as one: string and
         * number literals and IN (...) lists are replaced by ?. Parameters
         * are never logged. Each route and statement is logged at most once
         * a minute. 0, the default, turns the detector off.
         */
        public function repeatedQueries(int $times = 10): static
        {
            if ($times < 0 || $times === 1) {
                throw new \InvalidArgumentException('repeatedQueries must be 0 (off) or 2 or more');
            }
            $this->repeatedQueries = $times;
            return $this;
        }

        /** @internal */
        public function repeatedQueriesAt(): int
        {
            return $this->repeatedQueries;
        }

        /**
         * Log every request that takes longer than $seconds, with where its
         * time went, measured in Go with no code in the app:
         *
         *     slow request route="GET /orders/{id}" ms=1240 php_ms=40 longest_run_ms=12 sql="38 in 310ms" http="2 in 890ms"
         *
         * Kinds: sql (PDO pgsql, Cairo\Sql), http (Cairo\Http\Client, curl),
         * socket (other hooked sockets: PDO MySQL, Redis, streams), connect
         * (DNS, connect, TLS), sleep, wait (channels, select(), Cairo\Pool
         * with acquireTimeout), offload and grpc. Waits of async() tasks run side by side, so the kinds can
         * add up to more than the request. php_ms is the time when none of
         * the request's fibers was waiting in Go: running PHP, or queued
         * behind other requests on its thread. longest_run_ms is the longest
         * stretch of PHP it ran without yielding, while every other request on
         * its thread waited: move the work to offload() when it is high. 0
         * (the default) turns it off.
         */
        public function slowRequests(float $seconds = 1.0): static
        {
            if ($seconds < 0) {
                throw new \InvalidArgumentException('slowRequests must be 0 or more');
            }
            $this->slowRequests = $seconds;
            return $this;
        }

        /**
         * Log a warning for each route that runs PHP for longer than $seconds
         * without yielding (a CPU loop, a big json_encode, image work), since
         * every other request on its thread waits meanwhile:
         *
         *     level=WARN msg="heavy route: move its CPU work to offload()" route="POST /report" longest_run_ms=212
         *
         * Move that work to Cairo\offload(), which runs it on another thread.
         * Measured in Go with no code in the app, like slowRequests(), and
         * logged at most once a minute per route. Isolated mode, where
         * blocking calls are expected and would count as PHP, is not
         * reported. 0 (the default) turns it off.
         */
        public function heavyRoutes(float $seconds = 0.05): static
        {
            if ($seconds < 0) {
                throw new \InvalidArgumentException('heavyRoutes must be 0 or more');
            }
            $this->heavyRoutes = $seconds;
            return $this;
        }

        /**
         * Backpressure: at most $n requests in flight per request thread (0,
         * the default, means no cap). Further requests wait in Go, which costs
         * a goroutine, not PHP memory, until one finishes or their context
         * ends. Use it when each request holds a lot of memory.
         */
        public function maxInFlight(int $n): static
        {
            if ($n < 0) {
                throw new \InvalidArgumentException('maxInFlight must be 0 or more');
            }
            $this->maxInFlight = $n;
            return $this;
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
        public function register(mixed $handler, string $route = ''): int
        {
            $this->handlers[] = $handler;
            $this->routes[] = $route;
            return count($this->handlers) - 1;
        }

        /** @internal the route of handler $id, as logs name it */
        public function routeName(int $id): string
        {
            return $this->routes[$id] ?? '';
        }

        /** @internal */
        public function handler(int $id): mixed
        {
            return $this->handlers[$id] ?? throw new \LogicException("unknown handler id $id");
        }

        /** @internal */
        public function spec(): array
        {
            $spec = ['threads' => $this->threads, 'offload_threads' => $this->offloadThreads, 'detect_blocking' => $this->detectBlocking, 'slow_requests' => $this->slowRequests, 'heavy_routes' => $this->isolated() ? 0.0 : $this->heavyRoutes, 'max_in_flight' => $this->maxInFlight, 'isolated' => $this->isolated(), 'http' => [], 'grpc' => []];
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

        /** @internal the request's own context, which every other one derives from */
        public function root(): self
        {
            $root = $this;
            while ($root->parent !== null) {
                $root = $root->parent;
            }
            return $root;
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
            \cairo_local_bind($fiber, $context->requestId);
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

        /** Why the context ended (it, or a context it derives from), or null while it is live. */
        public function err(): ?Context\ContextException
        {
            for ($ctx = $this; $ctx !== null; $ctx = $ctx->parent) {
                if ($ctx->ended !== null) {
                    return $ctx->ended === 'canceled' ? new Context\Canceled() : new Context\DeadlineExceeded();
                }
                if ($ctx->deadline !== null && microtime(true) >= $ctx->deadline) {
                    return new Context\DeadlineExceeded();
                }
            }
            return null;
        }

        /** @var array<string, mixed> values Go middleware set on the request (root context only) */
        private array $goValues = [];
        /** this context came from withTimeout() and friends and owns its goroutine */
        private bool $derived = false;
        /** cancel() was called on it */
        private bool $canceledHere = false;
        /** @var ?\WeakMap<self, Channel> done() channels, woken when cancel() ends their context */
        private static ?\WeakMap $doneChannels = null;

        /** @internal */
        public function setGoValues(array $values): void
        {
            $this->goValues = $values;
        }

        /**
         * A value Go middleware attached to the request, such as 'request_id'
         * from Middleware::requestId(). Values PHP code stores are read with
         * Context::get().
         */
        public function value(string $key, mixed $default = null): mixed
        {
            $root = $this;
            while ($root->parent !== null) {
                $root = $root->parent;
            }
            return array_key_exists($key, $root->goValues) ? $root->goValues[$key] : $default;
        }

        /**
         * A child context that ends $seconds from now, or earlier if this one
         * ends. Run code under it with run(): every Cairo call it makes (HTTP,
         * gRPC, sockets, SQL, sleep, channels, async() tasks) ends with it.
         */
        public function withTimeout(float $seconds): self
        {
            return $this->derive(max(0.0, $seconds));
        }

        /** Like withTimeout(), with an absolute time. */
        public function withDeadline(\DateTimeInterface|float $at): self
        {
            $at = $at instanceof \DateTimeInterface ? (float) $at->format('U.u') : $at;
            return $this->derive(max(0.0, $at - microtime(true)));
        }

        /** A child context that ends when cancel() is called on it, or when this one ends. */
        public function withCancel(): self
        {
            return $this->derive(null);
        }

        private function derive(?float $timeout): self
        {
            if (!Internal\Scheduler::running()) {
                throw new \LogicException('derived contexts need a Cairo request thread');
            }
            $id = \cairo_context_open($this->fiberId, $timeout ?? -1.0);
            $deadline = $this->deadline;
            if ($timeout !== null) {
                $at = microtime(true) + $timeout;
                $deadline = $deadline === null ? $at : min($deadline, $at);
            }
            $child = new self($this->requestId, $id, $deadline, $this);
            $child->derived = true;
            return $child;
        }

        /**
         * End this context (and the ones derived from it): Cairo calls running
         * under it throw Context\Canceled, and its done() channel closes. Only
         * contexts made with withTimeout(), withDeadline() or withCancel() can
         * be canceled.
         */
        public function cancel(): void
        {
            if (!$this->derived) {
                throw new \LogicException('only a context made with withTimeout(), withDeadline() or withCancel() can be canceled');
            }
            if ($this->canceledHere) {
                return;
            }
            $this->canceledHere = true;
            $this->ended ??= 'canceled';
            \cairo_context_cancel($this->fiberId);
            foreach (self::$doneChannels ?? [] as $ch) {
                if ($ch->isClosed()) {
                    $ch->contextEnded();
                }
            }
        }

        /**
         * Run $fn with this context as the current one, in the current fiber;
         * returns what $fn returns. `$ctx->withTimeout(0.5)->run(fn () => $client->get($url))`.
         */
        public function run(callable $fn): mixed
        {
            $fiber = \Fiber::getCurrent() ?? throw new \LogicException('Context::run() must be called inside a request (a fiber)');
            $prev = self::current();
            self::bind($fiber, $this);
            try {
                return $fn($this);
            } finally {
                self::bind($fiber, $prev);
            }
        }

        public function __destruct()
        {
            if ($this->derived) {
                \cairo_fiber_close($this->fiberId);   // ends its goroutine and cancels its Go context
            }
        }

        /** @var ?string how Go reported this context ended: 'canceled' or 'deadline_exceeded' */
        private ?string $ended = null;
        private ?Channel $done = null;

        /**
         * A channel that is closed when this context ends (deadline passed or
         * canceled), for select(): `select(['job' => $jobs, 'done' => $ctx->done()])`.
         * It never carries values.
         */
        public function done(): Channel
        {
            if ($this->done === null) {
                $this->done = Channel::forContext($this);
                self::$doneChannels ??= new \WeakMap();
                self::$doneChannels[$this] = $this->done;
            }
            return $this->done;
        }

        /** @internal the deadline as a Unix time, or null */
        public function deadlineAt(): ?float
        {
            return $this->deadline;
        }

        /**
         * @internal Go reported that this fiber's context ended. Its parents
         * ended too when the cause is theirs: a cancel, or the deadline they share.
         */
        public function ended(string $why): void
        {
            for ($ctx = $this; $ctx !== null; $ctx = $ctx->parent) {
                if ($why === 'canceled' && $ctx->canceledHere) {
                    return;   // the cancel came from here: what it derives from goes on
                }
                if ($ctx->ended !== null) {
                    continue;
                }
                if ($why === 'canceled' || ($ctx->deadline !== null && $this->deadline !== null && $ctx->deadline <= $this->deadline)) {
                    $ctx->ended = $why;
                }
            }
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

    /**
     * A channel between fibers of one PHP thread, as in Go: send() waits
     * until a receiver takes the value (or, with a capacity, until there is
     * room), recv() waits for a value. Waiting suspends only the fiber, and
     * also ends when the request's context ends (Context\DeadlineExceeded or
     * Context\Canceled is thrown). After close(), receivers drain what is
     * buffered, then get Channel\Closed; send() throws Channel\Closed.
     *
     * @implements \IteratorAggregate<int, mixed>
     */
    final class Channel implements \IteratorAggregate, \Countable
    {
        /** @var \SplQueue<mixed> */
        private \SplQueue $buffer;
        private bool $closed = false;
        /** @var array<int, array{Internal\Waiter, int|string}> waiting receivers: object id => [waiter, case key] */
        private array $receivers = [];
        /** @var array<int, array{Internal\Waiter, int|string, mixed}> waiting senders: [waiter, case key, value] */
        private array $senders = [];
        private ?Context $context = null;

        public function __construct(private readonly int $capacity = 0)
        {
            if ($capacity < 0) {
                throw new \InvalidArgumentException('capacity must be 0 or more');
            }
            $this->buffer = new \SplQueue();
        }

        /** @internal the channel behind Context::done() */
        public static function forContext(Context $context): self
        {
            $ch = new self();
            $ch->context = $context;
            return $ch;
        }

        public function send(mixed $value): void
        {
            select([[$this, $value]]);
        }

        /** The next value; throws Channel\Closed once the channel is closed and drained. */
        public function recv(): mixed
        {
            [, $value, $ok] = select([$this]);
            if (!$ok) {
                throw new Channel\Closed();
            }
            return $value;
        }

        /** Like recv() without waiting: false when no value is ready. */
        public function tryRecv(mixed &$value = null): bool
        {
            [$key, $v, $ok] = select([$this], 0);
            if ($key === null || !$ok) {
                return false;
            }
            $value = $v;
            return true;
        }

        /** Like send() without waiting: false when the value cannot be taken now. */
        public function trySend(mixed $value): bool
        {
            return select([[$this, $value]], 0)[0] !== null;
        }

        public function close(): void
        {
            if ($this->context !== null) {
                throw new \LogicException('a context\'s done() channel closes when the context ends');
            }
            if ($this->closed) {
                return;
            }
            $this->closed = true;
            foreach ($this->receivers as [$waiter, $key]) {
                $waiter->wake($key, null, false);
            }
            foreach ($this->senders as [$waiter]) {
                $waiter->fail(new Channel\Closed());
            }
            $this->receivers = $this->senders = [];
        }

        public function isClosed(): bool
        {
            return $this->context !== null ? $this->context->err() !== null : $this->closed;
        }

        /** Values buffered now. */
        public function count(): int
        {
            return $this->buffer->count();
        }

        public function capacity(): int
        {
            return $this->capacity;
        }

        /** Receives until the channel is closed and drained. */
        public function getIterator(): \Generator
        {
            while (true) {
                [, $value, $ok] = select([$this]);
                if (!$ok) {
                    return;
                }
                yield $value;
            }
        }

        /** @internal a done() channel's context ended: wake whoever waits on it */
        public function contextEnded(): void
        {
            foreach ($this->receivers as [$waiter, $key]) {
                $waiter->wake($key, null, false);
            }
            $this->receivers = [];
        }

        /** @internal the context behind a done() channel */
        public function context(): ?Context
        {
            return $this->context;
        }

        /** @internal try to receive now: [true, value, ok] or [false] */
        public function pollRecv(): array
        {
            if (!$this->buffer->isEmpty()) {
                $value = $this->buffer->dequeue();
                // Room now: the longest waiting sender fills it.
                if (($s = $this->takeSender()) !== null) {
                    $this->buffer->enqueue($s[2]);
                    $s[0]->wake($s[1], null, true);
                }
                return [true, $value, true];
            }
            if (($s = $this->takeSender()) !== null) {
                $s[0]->wake($s[1], null, true);
                return [true, $s[2], true];
            }
            if ($this->isClosed()) {
                return [true, null, false];
            }
            return [false];
        }

        /** @internal try to send now: whether the value was taken */
        public function pollSend(mixed $value): bool
        {
            if ($this->context !== null) {
                throw new \LogicException('cannot send on a context\'s done() channel');
            }
            if ($this->closed) {
                throw new Channel\Closed();
            }
            if (($r = $this->takeReceiver()) !== null) {
                $r[0]->wake($r[1], $value, true);
                return true;
            }
            if ($this->buffer->count() < $this->capacity) {
                $this->buffer->enqueue($value);
                return true;
            }
            return false;
        }

        /** @internal */
        public function addReceiver(Internal\Waiter $waiter, int|string $key): void
        {
            $this->receivers[spl_object_id($waiter)] = [$waiter, $key];
        }

        /** @internal */
        public function addSender(Internal\Waiter $waiter, int|string $key, mixed $value): void
        {
            $this->senders[spl_object_id($waiter)] = [$waiter, $key, $value];
        }

        /** @internal */
        public function remove(Internal\Waiter $waiter): void
        {
            unset($this->receivers[spl_object_id($waiter)], $this->senders[spl_object_id($waiter)]);
        }

        private function takeSender(): ?array
        {
            foreach ($this->senders as $id => $s) {
                unset($this->senders[$id]);
                if (!$s[0]->done) {
                    return $s;
                }
            }
            return null;
        }

        private function takeReceiver(): ?array
        {
            foreach ($this->receivers as $id => $r) {
                unset($this->receivers[$id]);
                if (!$r[0]->done) {
                    return $r;
                }
            }
            return null;
        }
    }

    /**
     * Wait on several channel operations and do the first one that can go,
     * as Go's select. Each case is a Channel (receive) or [Channel, value]
     * (send); keys name the cases. Returns [key, value, ok]: the received
     * value (null for a send), and ok false when a receive found the channel
     * closed (a context's done() channel included). When several cases are
     * ready, one is picked at random.
     *
     * $timeout in seconds: null waits as long as the context allows, 0 never
     * waits. When it passes, select() returns [null, null, false]. If the
     * context ends first and no case is ready, Context\DeadlineExceeded or
     * Context\Canceled is thrown; add `$ctx->done()` as a case to get it as
     * a case instead.
     *
     * @param array<int|string, Channel|array{Channel, mixed}> $cases
     * @return array{int|string|null, mixed, bool}
     */
    function select(array $cases, ?float $timeout = null): array
    {
        if ($cases === [] && $timeout === null) {
            throw new \InvalidArgumentException('select() with no cases and no timeout would wait forever');
        }
        foreach ($cases as $key => $case) {
            if (!$case instanceof Channel && !(is_array($case) && count($case) === 2 && ($case[0] ?? null) instanceof Channel)) {
                throw new \InvalidArgumentException("select() case '$key' must be a Channel or [Channel, value]");
            }
        }
        $poll = static function () use ($cases): ?array {
            $keys = array_keys($cases);
            shuffle($keys);
            foreach ($keys as $key) {
                $case = $cases[$key];
                if ($case instanceof Channel) {
                    $r = $case->pollRecv();
                    if ($r[0]) {
                        return [$key, $r[1], $r[2]];
                    }
                } elseif ($case[0]->pollSend($case[1])) {
                    return [$key, null, true];
                }
            }
            return null;
        };
        if (($ready = $poll()) !== null) {
            return $ready;
        }
        if ($timeout !== null && $timeout <= 0) {
            return [null, null, false];
        }
        $fiber = \Fiber::getCurrent();
        $ctx = Context::current();
        if ($fiber === null || $ctx->fiberId === 0 || !Internal\Scheduler::running()) {
            throw new \LogicException('waiting on a Cairo\Channel must happen inside a request handler or an async() task');
        }

        // done() channels of other contexts close at their deadline, which the
        // park below does not see: wake up then and look again.
        $end = $timeout === null ? null : microtime(true) + $timeout;
        while (true) {
            $now = microtime(true);
            $doneAt = null;
            foreach ($cases as $case) {
                if ($case instanceof Channel && ($at = $case->context()?->deadlineAt()) !== null && $at > $now) {
                    $doneAt = $doneAt === null ? $at : min($doneAt, $at);
                }
            }
            $wait = $end === null ? -1.0 : max(0.0, $end - microtime(true));
            $forDone = false;
            if ($doneAt !== null && ($wait < 0 || $doneAt - microtime(true) < $wait)) {
                $wait = max(0.0, $doneAt - microtime(true)) + 0.001;
                $forDone = true;
            }
            $waiter = new Internal\Waiter();
            foreach ($cases as $key => $case) {
                if ($case instanceof Channel) {
                    $case->addReceiver($waiter, $key);
                } else {
                    $case[0]->addSender($waiter, $key, $case[1]);
                }
            }
            try {
                $event = Internal\Scheduler::instance()->park($waiter, $wait);
            } finally {
                foreach ($cases as $case) {
                    ($case instanceof Channel ? $case : $case[0])->remove($waiter);
                }
            }
            if ($waiter->done) {
                if ($waiter->error !== null) {
                    throw $waiter->error;
                }
                return $waiter->result;
            }
            if (isset($event['error'])) {
                $ctx->ended((string) $event['error']);
                // A done() channel among the cases is now closed.
                if (($ready = $poll()) !== null) {
                    return $ready;
                }
                throw $event['error'] === 'canceled' ? new Context\Canceled() : new Context\DeadlineExceeded();
            }
            if (!$forDone) {
                return [null, null, false];   // timeout
            }
            if (($ready = $poll()) !== null) {
                return $ready;
            }
        }
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
        return Internal\spawnTask(Context::current(), $fn);
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
     * A job for offload() that carries its own input: the object is
     * serialized, sent to an offload thread and run() there. Its class must be
     * loadable on every thread (declared or autoloaded from server.php).
     *
     *     final class Thumbnail implements OffloadTask {
     *         public function __construct(private string $path, private int $width) {}
     *         public function run(): string { ... }
     *     }
     *     $png = offload(new Thumbnail($path, 200));
     */
    interface OffloadTask
    {
        public function run(): mixed;
    }

    /**
     * Run CPU-heavy work on the offload thread pool, so this request thread
     * keeps serving other fibers. The calling fiber suspends until it is done.
     *
     * Code cannot move between PHP threads, so $fn must be an OffloadTask
     * object, a function name or [ClassName::class, 'staticMethod'] that
     * server.php makes available, and the task, $args and the result must be
     * serializable. Closures are not accepted.
     *
     * @param OffloadTask|callable-string|array{0:class-string,1:string} $fn
     */
    function offload(OffloadTask|string|array $fn, mixed ...$args): mixed
    {
        if ($fn instanceof OffloadTask && $args !== []) {
            throw new \InvalidArgumentException('offload() takes no arguments with an OffloadTask; give them to the task');
        }
        if (\Fiber::getCurrent() === null || !Scheduler::running()) {
            // already off the request threads
            return $fn instanceof OffloadTask ? $fn->run() : $fn(...$args);
        }
        if (!$fn instanceof OffloadTask && !is_callable($fn)) {
            throw new \InvalidArgumentException('offload() needs an OffloadTask, a function name or [Class::class, "staticMethod"]');
        }
        $payload = base64_encode(serialize([$fn, $args]));
        $result = Scheduler::instance()->call('offload', $payload);
        $out = unserialize(base64_decode((string) $result));
        if (!($out['ok'] ?? false)) {
            throw new OffloadException(($out['class'] ?? 'Error') . ': ' . ($out['message'] ?? 'offloaded job failed'));
        }
        return $out['value'];
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

namespace Cairo\Channel {

    /** The channel was closed: no more sends, and nothing left to receive. */
    final class Closed extends \RuntimeException
    {
        public function __construct()
        {
            parent::__construct('channel closed');
        }
    }
}

namespace Cairo\Http {

    use Cairo\App;

    final class Server
    {
        /** @var list<array{id:int,method:string,path:string,cache?:array{ttl:float,stale_while_revalidate:float,vary:list<string>,tags:list<string>}}> */
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
            $id = $this->app->register($handler, $name);
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
     * has finished; send() sends one reply. Generated interfaces type it as
     * ServerStream<Request, Reply>.
     *
     * @template TRequest of array<string, mixed>|object
     * @template TReply of array<string, mixed>|object
     * @implements \IteratorAggregate<int, TRequest>
     */
    final class ServerStream implements \IteratorAggregate
    {
        /** @internal */
        public function __construct(private readonly int $id, private readonly ?string $class = null)
        {
        }

        /** @return TRequest|null */
        public function recv(): array|object|null
        {
            $r = Stream::call('grpc.server.recv', ['id' => $this->id]);
            if (!empty($r['eof'])) {
                return null;
            }
            return Stream::decode($r['message'], $this->class);
        }

        /** @param TReply $message */
        public function send(array|object $message): void
        {
            // Queued without a fiber switch while Go's reply queue has room;
            // a full queue waits on the fiber's goroutine instead.
            $payload = ['id' => $this->id, 'message' => Stream::encode($message)];
            $r = \Cairo\Internal\Go::callNow('grpc.server.trySend', $payload);
            if (!is_array($r) || !($r['ok'] ?? false)) {
                throw new Exception((int) ($r['code'] ?? Code::UNKNOWN), (string) ($r['error'] ?? 'stream failed'));
            }
            if (!($r['queued'] ?? false)) {
                Stream::call('grpc.server.send', $payload);
            }
        }

        /** @return \Generator<int, TRequest> */
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
     * throws Cairo\Grpc\Exception). Iterating reads the replies. Generated
     * clients type it as ClientStream<Request, Reply>.
     *
     * @template TRequest of array<string, mixed>|object
     * @template TReply of array<string, mixed>|object
     * @implements \IteratorAggregate<int, TReply>
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

        /** @param TRequest $message */
        public function send(array|object $message): void
        {
            Stream::call('grpc.stream.send', ['id' => $this->id, 'message' => Stream::encode($message)]);
        }

        /** No more requests: the server sees the end of the client's stream. */
        public function closeSend(): void
        {
            Stream::call('grpc.stream.closeSend', ['id' => $this->id]);
        }

        /** @return TReply|null */
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

        /**
         * For client-streaming methods: end the requests and read the one reply.
         *
         * @return TReply
         */
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

        /** @return \Generator<int, TReply> */
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

namespace Cairo\Sql {

    use Cairo\Internal\Go;

    /**
     * A SQL query failed. $sqlState is the five-character SQLSTATE code
     * (e.g. 23505 for a unique violation), when the database sent one.
     */
    class SqlException extends \RuntimeException
    {
        public function __construct(string $message, public readonly ?string $sqlState = null, public readonly ?string $detail = null)
        {
            parent::__construct($message);
        }
    }

    /** A binary parameter (bytea, BLOB). Values read back from binary columns are plain strings. */
    final class Bytes
    {
        public function __construct(public readonly string $bytes)
        {
        }
    }

    /** What Db and Tx share: running statements. */
    abstract class Queryable
    {
        /** @return array<string, int> which pool or transaction runs the statement */
        abstract protected function target(): array;

        /**
         * Run a query and return every row as column => value. Parameters
         * are $1, $2, ... for PostgreSQL and ? for MySQL. Only the calling
         * fiber waits.
         *
         * @param list<mixed> $params
         * @return list<array<string, mixed>>
         */
        public function query(string $sql, array $params = []): array
        {
            $res = $this->run($sql, $params, false);
            $columns = $res['columns'];
            $rows = [];
            foreach ($res['rows'] as $row) {
                $rows[] = array_combine($columns, array_map(self::decode(...), $row));
            }
            return $rows;
        }

        /**
         * The first row, or null when there is none.
         *
         * @param list<mixed> $params
         * @return array<string, mixed>|null
         */
        public function queryRow(string $sql, array $params = []): ?array
        {
            return $this->query($sql, $params)[0] ?? null;
        }

        /**
         * The first column of the first row, or null when there is no row.
         *
         * @param list<mixed> $params
         */
        public function queryValue(string $sql, array $params = []): mixed
        {
            $row = $this->queryRow($sql, $params);
            return $row === null ? null : reset($row);
        }

        /**
         * Run a statement and return how many rows it changed.
         *
         * @param list<mixed> $params
         */
        public function exec(string $sql, array $params = []): int
        {
            return (int) $this->run($sql, $params, true)['affected'];
        }

        /** @param list<mixed> $params */
        private function run(string $sql, array $params, bool $exec): array
        {
            $args = [];
            foreach (array_values($params) as $i => $p) {
                $args[] = match (true) {
                    $p instanceof Bytes => ['$bytes' => base64_encode($p->bytes)],
                    $p instanceof \DateTimeInterface => ['$time' => $p->format('Y-m-d\TH:i:s.uP')],
                    $p instanceof \BackedEnum => $p->value,
                    $p instanceof \Stringable => (string) $p,
                    is_array($p) || is_object($p) => throw new \InvalidArgumentException(sprintf('parameter %d: %s is not a SQL value; json_encode() it', $i + 1, get_debug_type($p))),
                    default => $p,
                };
            }
            if (\Cairo\Internal\Scheduler::watchingQueries()) {
                \Cairo\Internal\Queries::seen($sql);
            }
            return Db::call('sql.query', $this->target() + ['sql' => $sql, 'args' => $args, 'exec' => $exec]);
        }

        private static function decode(mixed $v): mixed
        {
            return is_array($v) && isset($v['$bytes']) ? base64_decode($v['$bytes']) : $v;
        }
    }

    /**
     * A PostgreSQL or MySQL connection pool in Go (database/sql with pgx or
     * go-sql-driver/mysql), shared by every PHP thread: Db::open() with the
     * same DSN and options returns the same pool on each thread, so max_open
     * bounds the connections of the whole server.
     *
     *     $db = Db::open('postgres://app:secret@127.0.0.1/app', ['max_open' => 20]);
     *     $users = $db->query('SELECT id, name FROM users WHERE team = $1', [$team]);
     *
     *     $db = Db::open('mysql://app:secret@127.0.0.1:3306/app');
     *     $users = $db->query('SELECT id, name FROM users WHERE team = ?', [$team]);
     *
     * Integers, floats, booleans, strings and nulls come back as such; binary
     * columns as strings, timestamps as RFC 3339 in UTC (MySQL DATE as
     * Y-m-d), numeric/DECIMAL, json and the rest in the database's text form.
     */
    final class Db extends Queryable
    {
        private function __construct(private readonly int $pool)
        {
        }

        /**
         * $dsn is a postgres:// URL, a PostgreSQL key=value DSN, or
         * mysql://user:pass@host:port/db (?socket=/path for a Unix socket,
         * plus the go-sql-driver/mysql parameters such as tls or timeout).
         *
         * Options: max_open (connections, 0 for no limit; default 10),
         * max_idle (default max_open, or 10 with no limit), max_lifetime and
         * max_idle_time (seconds). Fewer idle than open connections means a
         * reconnect for most queries under load; max_idle_time closes the
         * ones a quiet pool does not need. acquire_timeout (seconds) bounds
         * the wait for a free connection with a Cairo\PoolTimeout; without
         * it a query waits until its request ends. Connecting is lazy: a bad
         * DSN shows on the first query.
         *
         * Statements that would leave a transaction or a lock on a pooled
         * connection (BEGIN, START TRANSACTION, SET autocommit, LOCK, XA)
         * throw a SqlException: use transaction() instead.
         *
         * @param array{max_open?: int, max_idle?: int, max_lifetime?: float, max_idle_time?: float, acquire_timeout?: float} $options
         */
        public static function open(string $dsn, array $options = []): self
        {
            $maxOpen = (int) ($options['max_open'] ?? 10);
            $pool = self::call('sql.open', [
                'dsn' => $dsn,
                'max_open' => $maxOpen,
                'max_idle' => (int) ($options['max_idle'] ?? ($maxOpen > 0 ? $maxOpen : 10)),
                'max_lifetime' => (float) ($options['max_lifetime'] ?? 0),
                'max_idle_time' => (float) ($options['max_idle_time'] ?? 0),
                'acquire_timeout' => (float) ($options['acquire_timeout'] ?? 0),
            ]);
            return new self((int) $pool);
        }

        /**
         * Run $fn in a transaction: committed when $fn returns, rolled back
         * when it throws (the exception is rethrown). Returns what $fn returns.
         * A transaction left open when its fiber ends is rolled back.
         *
         * @template T
         * @param callable(Tx): T $fn
         * @param array{isolation?: 'read committed'|'repeatable read'|'serializable', read_only?: bool} $options
         * @return T
         */
        public function transaction(callable $fn, array $options = []): mixed
        {
            $tx = new Tx((int) self::call('sql.begin', [
                'pool' => $this->pool,
                'isolation' => (string) ($options['isolation'] ?? ''),
                'read_only' => (bool) ($options['read_only'] ?? false),
            ]));
            try {
                $result = $fn($tx);
            } catch (\Throwable $e) {
                $tx->end(false);
                throw $e;
            }
            $tx->end(true);
            return $result;
        }

        /** @return array{max_open: int, open: int, in_use: int, idle: int, wait_count: int, wait_time: float} */
        public function stats(): array
        {
            return self::call('sql.stats', ['pool' => $this->pool]);
        }

        protected function target(): array
        {
            return ['pool' => $this->pool];
        }

        /** @internal */
        public static function call(string $op, array $payload): mixed
        {
            $res = Go::call($op, $payload);
            if (is_array($res) && isset($res['pool_timeout'])) {
                throw new \Cairo\PoolTimeout((string) $res['error']);
            }
            if (is_array($res) && isset($res['error'])) {
                throw new SqlException((string) $res['error'], $res['sqlstate'] ?? null, $res['detail'] ?? null);
            }
            return $res;
        }
    }

    /** A transaction; see Db::transaction(). */
    final class Tx extends Queryable
    {
        private bool $ended = false;

        /** @internal */
        public function __construct(private readonly int $tx)
        {
        }

        protected function target(): array
        {
            if ($this->ended) {
                throw new SqlException('the transaction has ended');
            }
            return ['tx' => $this->tx];
        }

        /** @internal commit (true) or roll back */
        public function end(bool $commit): void
        {
            if ($this->ended) {
                return;
            }
            $this->ended = true;
            try {
                Db::call('sql.end', ['tx' => $this->tx, 'commit' => $commit]);
            } catch (SqlException $e) {
                if ($commit) {
                    throw $e;
                }
            }
        }
    }
}

namespace Cairo\Internal {

    use Cairo\App;
    use Cairo\Context;
    use Cairo\Grpc;
    use Cairo\Http;

    /** One per PHP thread: runs every request in its own fiber. */
    /** @internal a fiber waiting in select() */
    final class Waiter
    {
        public int $op = 0;
        public bool $done = false;
        /** @var array{int|string|null, mixed, bool} */
        public array $result = [null, null, false];
        public ?\Throwable $error = null;

        /** Another fiber completed one of this waiter's cases. */
        public function wake(int|string $key, mixed $value, bool $ok): void
        {
            if ($this->done) {
                return;
            }
            $this->done = true;
            $this->result = [$key, $value, $ok];
            \cairo_wake($this->op);
        }

        public function fail(\Throwable $error): void
        {
            if ($this->done) {
                return;
            }
            $this->done = true;
            $this->error = $error;
            \cairo_wake($this->op);
        }
    }

    final class Scheduler
    {
        private static ?self $instance = null;

        /** @var array<int, \Fiber> I/O operation id => waiting fiber */
        private array $io = [];
        private \SplQueue $ready;
        /** @var \SplQueue<\Closure> work to start from this loop, outside any fiber */
        private \SplQueue $later;
        /** @var array<int, mixed> handler id => resolved handler */
        private array $resolved = [];

        /** Isolated mode: requests waiting for the one in progress to finish. */
        private \SplQueue $pending;
        private bool $busy = false;

        /** Thread recycling (App::maxRequests(), App::maxMemory()). */
        private int $active = 0;
        private int $served = 0;

        /** Idle fibers kept by startFiber(), at most IDLE_FIBERS. */
        private const IDLE_FIBERS = 64;
        /** @var list<\Fiber> */
        private array $idle = [];
        /** A task failed or a request ended early since the last collectAfterFailure(). */
        private bool $failed = false;
        private float $lastCollect = 0.0;
        private bool $retireAsked = false;
        private bool $retiring = false;

        /** App::repeatedQueries() is on: PDO and Cairo\Sql report their statements */
        private static bool $watchQueries = false;

        public function __construct(private readonly App $app)
        {
            $this->ready = new \SplQueue();
            $this->later = new \SplQueue();
            $this->pending = new \SplQueue();
            self::$instance = $this;
            self::$watchQueries = $app->repeatedQueriesAt() > 0;
            \cairo_watch_queries(self::$watchQueries);
        }

        /** @internal */
        public static function watchingQueries(): bool
        {
            return self::$watchQueries;
        }

        /** @param array<string, mixed> $event */
        private function routeOf(array $event): string
        {
            if ($event['type'] === 'http') {
                return $this->app->routeName((int) $event['route']);
            }
            return $this->app->routeName((int) $event['service']) . '/' . (string) $event['method'];
        }

        public static function instance(): self
        {
            return self::$instance ?? throw new \LogicException('the Cairo scheduler is not running');
        }

        /**
         * Run $job in a fiber bound to $context. A fiber whose job finished is
         * kept and runs the next job: a new fiber costs a stack mmap and, when
         * it is freed, a TLB shootdown on every CPU.
         */
        public function startFiber(\Closure $job, Context $context): void
        {
            while (($fiber = array_pop($this->idle)) !== null && !$fiber->isSuspended()) {
            }
            if ($fiber !== null) {
                Context::bind($fiber, $context);
                $fiber->resume($job);
                return;
            }
            $fiber = new \Fiber(function (\Closure $job): void {
                do {
                    $job();
                    unset($job);   // an idle fiber must not keep the last job's captures alive
                    \cairo_local_bind(\Fiber::getCurrent(), 0);   // nor its request's statics
                    if (count($this->idle) >= self::IDLE_FIBERS) {
                        return;
                    }
                    $this->idle[] = \Fiber::getCurrent();
                } while (($job = \Fiber::suspend()) instanceof \Closure);
            });
            Context::bind($fiber, $context);
            $fiber->start($job);
        }

        public static function running(): bool
        {
            return self::$instance !== null;
        }

        /** Ask the current fiber's goroutine to perform $op, and suspend until it is done. */
        public function op(array $op): mixed
        {
            return $this->submitted(\cairo_fiber_op(Context::current()->fiberId, json_encode($op, JSON_THROW_ON_ERROR)));
        }

        /** Ask the current fiber's goroutine to run $op with a payload string, and suspend until it is done. */
        public function call(string $op, string $payload): mixed
        {
            return $this->submitted(\cairo_fiber_call(Context::current()->fiberId, $op, $payload));
        }

        /** Wait for the operation a cairo_fiber_*() call just submitted as $id (-1: refused). */
        public function submitted(int $id): mixed
        {
            if ($id < 0) {
                throw new \LogicException('Cairo I/O must run inside a request handler or an async() task');
            }
            return $this->waitIO($id);
        }

        /**
         * Suspend the current fiber in select() until a Waiter::wake(), the
         * timeout (seconds, < 0 for none) or the end of its context. Returns
         * the I/O event, with 'error' when the context ended.
         */
        public function park(Waiter $waiter, float $timeout): array
        {
            $id = \cairo_fiber_op(Context::current()->fiberId, json_encode(['op' => 'park', 'timeout' => $timeout], JSON_THROW_ON_ERROR));
            if ($id < 0) {
                throw new \LogicException('waiting on a Cairo\Channel must happen inside a request handler or an async() task');
            }
            $waiter->op = $id;
            $this->io[$id] = \Fiber::getCurrent();
            return \Fiber::suspend();
        }

        /** Suspend the current fiber until Go reports I/O operation $id. */
        public function waitIO(int $id): mixed
        {
            $this->io[$id] = \Fiber::getCurrent() ?? throw new \LogicException('I/O must run inside a fiber');
            $event = \Fiber::suspend();
            if (isset($event['error'])) {
                if ($event['error'] === 'deadline_exceeded' || $event['error'] === 'canceled') {
                    Context::current()->ended($event['error']);
                }
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

        /**
         * Run $fn from the scheduler's loop, once the running fiber yields.
         * Safe where switching fibers is not, such as in a destructor run by
         * the garbage collector.
         */
        public function later(\Closure $fn): void
        {
            $this->later->enqueue($fn);
        }

        public function run(): void
        {
            while (true) {
                do {
                    while (!$this->ready->isEmpty() || !$this->later->isEmpty()) {
                        if (!$this->ready->isEmpty()) {
                            $this->ready->dequeue()->resume();
                        } else {
                            ($this->later->dequeue())();
                        }
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
                $event = \cairo_next_event();
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
            $context->setGoValues((array) ($event['values'] ?? []));
            $this->active++;
            $this->startFiber(function () use ($fn, $context, $event): void {
                try {
                    $fn();
                } finally {
                    $context->finish();
                    \cairo_local_end($context->requestId);
                    if (self::$watchQueries) {
                        Queries::end($context->requestId, $this->routeOf($event), $this->app->repeatedQueriesAt());
                    }
                    $this->active--;
                    $this->served++;
                    if ($context->err() !== null) {
                        $this->failed = true;
                    }
                    $this->collectAfterFailure();
                    $this->checkRecycle();
                }
            }, $context);
        }

        /**
         * @internal Wait for socket $fd on the thread itself for a moment (see
         * cairo_wait_here() in waitops.c), when nothing else could run on it
         * meanwhile: no fiber is ready or waiting for Go and, unless the
         * thread serves one request at a time anyway, Go has no event queued
         * for it. Returns whether the socket became ready.
         */
        public function waitHere(int $fd, bool $write): bool
        {
            return $this->io === [] && $this->ready->isEmpty() && $this->later->isEmpty()
                && \cairo_wait_here($fd, $write, !$this->busy);
        }

        /** @internal An async() task threw. */
        public function taskFailed(): void
        {
            $this->failed = true;
        }

        /**
         * A failed task's exception trace, or a cancelled request's half-done
         * objects, often sit in a reference cycle holding curl handles and their
         * descriptors. PHP frees cycles only after 10,000 possible roots, so
         * collect them now, at most once a second so a burst of failures does
         * not collect for each one.
         */
        private function collectAfterFailure(): void
        {
            if (!$this->failed) {
                return;
            }
            $now = hrtime(true) / 1e9;
            if ($now - $this->lastCollect < 1.0) {
                return;
            }
            $this->failed = false;
            $this->lastCollect = $now;
            gc_collect_cycles();
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
            if (is_object($handler) && !$handler instanceof \Closure) {
                $handler = Http\Adapter::wrap($handler);
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
            } catch (Context\Canceled) {
                // The client went away; nobody reads this, and it is not an error.
                $response = Http\Response::text("Client Closed Request\n", 499);
            } catch (\Throwable $e) {
                error_log('cairo: ' . $e);
                $response = Http\Response::text("Internal Server Error\n", 500);
            }
            Context::current()->markAnswered();
            Form::untrack($event['files'] ?? []);   // Go removes what is left once we respond
            \cairo_respond_http((int) $event['id'], $response->status, $response->headers, $response->body);
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
            if (Scheduler::running() && Context::current()->fiberId !== 0) {
                $json = json_encode($payload, JSON_THROW_ON_ERROR | JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE);
                return Scheduler::instance()->call($op, $json);
            }
            return self::callNow($op, $payload);
        }

        /**
         * Run $op on this thread without suspending the fiber: only for ops
         * that never wait.
         *
         * @param array<string, mixed> $payload
         */
        public static function callNow(string $op, array $payload): mixed
        {
            $json = json_encode($payload, JSON_THROW_ON_ERROR | JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE);
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
            $g = self::build($event);
            $_GET = $g['get'];
            $_POST = $g['post'];
            $_COOKIE = $g['cookies'];
            $_SERVER = $g['server'];
            $_REQUEST = array_merge($g['get'], $g['post']);
            $_FILES = $g['files'];
        }

        /**
         * What the superglobals would hold for this request.
         * @return array{get: array, post: array, cookies: array, server: array, files: array}
         */
        public static function build(array $event): array
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
            return [
                'get' => $get,
                'post' => Form::fields($event),
                'cookies' => $cookies,
                'server' => $server,
                'files' => Form::superglobal($event['files'] ?? []),
            ];
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
                        parse_str((string) ($event['body'] ?? ''), $fields);
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
            $scheduler = Scheduler::instance();
            if ($scheduler->waitHere($fd, $write)) {
                return 1;
            }
            $result = $scheduler->submitted(\cairo_fiber_wait(Context::current()->fiberId, $fd, $write, $timeout));
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
            Scheduler::instance()->submitted(\cairo_fiber_waitset(Context::current()->fiberId, $read, $write, $timeout));
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
            $result = Scheduler::instance()->call('pg', $request);
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
                $event = \cairo_next_event();
                if ($event['type'] === 'shutdown') {
                    return;
                }
                if ($event['type'] !== 'offload') {
                    continue;
                }
                try {
                    [$fn, $args] = unserialize(base64_decode($event['payload']));
                    $out = ['ok' => true, 'value' => $fn instanceof \Cairo\OffloadTask ? $fn->run() : $fn(...$args)];
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
/** @internal async(), with the parent context given: start $fn in a new fiber with its own goroutine. */
    function spawnTask(Context $parent, callable $fn): \Cairo\Task
    {
        $task = new \Cairo\Task();
        // Go starts the goroutine that will serve this fiber.
        $goroutine = \cairo_fiber_open($parent->fiberId);
        $context = $parent->child($goroutine);
        Scheduler::instance()->startFiber(static function () use ($fn, $task, $goroutine, $context): void {
            try {
                $task->result = \cairo_guard($fn);
            } catch (\Throwable $e) {
                $task->error = $e;
                Scheduler::instance()->taskFailed();
            } finally {
                $context->finish();
                \cairo_fiber_close($goroutine);
            }
            $task->done = true;
            foreach ($task->waiters as $waiter) {
                Scheduler::instance()->ready($waiter);
            }
            $task->waiters = [];
        }, $context);
        return $task;
    }

    function interrupt(?string $file): bool
    {
        if ($file === __FILE__) {
            return false;   // never inside Cairo's runtime (it answers the request); Go pokes again
        }
        $context = \Cairo\Context::current();
        return !$context->answered() && $context->err() instanceof \Cairo\Context\DeadlineExceeded;
    }

    /**
     * Called inside the running fiber when Go's blocking detector found the
     * thread busy for $ms milliseconds (see cairo_report_blocking in cairo.c).
     */
    function reportBlocking(int $ms): void
    {
        if (!Scheduler::running() || Scheduler::instance()->app()->isolated()) {
            return;
        }
        // Frame i holds the line that called frame i's function, so the
        // function that line is in is frame i+1's. Frame 0's line is where
        // the request's code was when the interrupt ran.
        $trace = debug_backtrace(DEBUG_BACKTRACE_IGNORE_ARGS);
        $lines = [];
        foreach ($trace as $i => $frame) {
            if (!isset($frame['file']) || $frame['file'] === __FILE__) {
                continue;   // internal calls, and Cairo's runtime
            }
            $in = $trace[$i + 1] ?? null;
            $fn = $in === null ? '{main}' : (isset($in['class']) ? $in['class'] . $in['type'] . $in['function'] : $in['function']) . '()';
            $lines[] = sprintf('  %s:%d in %s', $frame['file'], $frame['line'] ?? 0, $fn);
        }
        $request = \Cairo\Context::current()->requestId;
        error_log("cairo: blocking: request $request kept its PHP thread busy for {$ms}ms; other requests on the thread waited. At:\n" . implode("\n", $lines));
    }

    function main(): void
    {
        $info = json_decode(\cairo_info(), true, 512, JSON_THROW_ON_ERROR);
        // Cairo\Revolt\Driver implements Revolt's interface, so it can only be
        // declared once revolt/event-loop is loadable: on first use.
        spl_autoload_register(static function (string $class): void {
            $file = [
                'Cairo\\Cache' => 'cache.php', 'Cairo\\Pool' => 'pool.php', 'Cairo\\PoolTimeout' => 'pool.php',
                'Cairo\\Internal\\Queries' => 'queries.php', 'Cairo\\RequestLocal' => 'request_local.php',
            ][$class] ?? null;
            if ($file !== null) {
                require_once __DIR__ . '/' . $file;
                return;
            }
            if (str_starts_with($class, 'Cairo\\Revolt\\')) {
                if (!interface_exists(\Revolt\EventLoop\Driver::class)) {
                    throw new \LogicException("$class needs revolt/event-loop: composer require revolt/event-loop, and load vendor/autoload.php first");
                }
                require_once __DIR__ . '/revolt.php';
            }
        });
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
