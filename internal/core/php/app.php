<?php
/**
 * Cairo\App: what server.php returns. It declares the PHP threads, the
 * concurrency mode, the limits and the servers; Go reads it once, at boot.
 */

declare(strict_types=1);

namespace Cairo {

    final class App
    {
        /** @var array<string, list<Http\Server|Grpc\Server>> the name of an extension => the servers declared with it */
        private array $servers = ['http' => [], 'grpc' => []];
        private int $threads = 0;
        private int $offloadThreads = 0;
        private int $maxRequests = 0;
        private int $maxMemory = 0;
        private float $detectBlocking = 0.0;
        private float $slowRequests = 0.0;
        private float $heavyRoutes = 0.0;
        private int $maxInFlight = 0;
        private bool $detectLeaks = false;
        /** @var list<string> "Class::$property" the leak detector skips */
        private array $leaksIgnored = [];
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
         * Development aid: log the static properties that hold request state
         * but are not request-local (see Cairo\RequestLocal), so one request
         * could read another's value:
         *
         *     level=WARN msg="static changed by a request" route="GET /orders" static=App\Tenant::$current
         *
         * When a request ends, every static property of the app's classes is
         * compared with its value at the previous request's end, a few
         * levels into arrays and objects. A static that is set once and then
         * kept (a lazy cache) is not reported; one that changes again on a
         * later request is. Registered statics and Cairo's own are skipped,
         * and so are those in $ignore: caches that grow but hold no request's
         * state, such as 'Illuminate\Support\Str::$snakeCache'. Each static
         * is logged at most once a minute. Keep it off in production: it
         * reads every static on every request, about 3 ms on a Laravel app.
         *
         * @param list<string> $ignore "Class::$property"
         */
        public function detectLeaks(bool $on = true, array $ignore = []): static
        {
            $this->detectLeaks = $on;
            $this->leaksIgnored = $ignore;
            return $this;
        }

        /**
         * @internal
         * @return ?list<string> the statics to skip, or null when the detector is off
         */
        public function leaksIgnored(): ?array
        {
            return $this->detectLeaks ? $this->leaksIgnored : null;
        }

        /**
         * Log every request that takes longer than $seconds, with where its
         * time went, measured in Go with no code in the app:
         *
         *     slow request route="GET /orders/{id}" ms=1240 php_ms=40 longest_run_ms=12 sql="38 in 310ms" http="2 in 890ms"
         *
         * Kinds: sql (PDO pgsql), http (curl),
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
            $this->servers['http'][] = $server;
            return $server;
        }

        public function grpc(string $listen): Grpc\Server
        {
            $server = new Grpc\Server($this, $listen);
            $this->servers['grpc'][] = $server;
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
            // Go's core reads the settings; what an extension declared goes
            // under its name, for the extension's Go half to read.
            $spec = ['threads' => $this->threads, 'offload_threads' => $this->offloadThreads, 'max_in_flight' => $this->maxInFlight, 'isolated' => $this->isolated(), 'extensions' => []];
            foreach ($this->servers as $extension => $servers) {
                $spec['extensions'][$extension] = array_map(static fn (Http\Server|Grpc\Server $server): array => $server->spec(), $servers);
            }
            $spec['extensions']['timing'] = ['slow_requests' => $this->slowRequests, 'heavy_routes' => $this->isolated() ? 0.0 : $this->heavyRoutes];
            $spec['extensions']['blocking'] = ['after' => $this->detectBlocking];
            return $spec;
        }
    }

    /** A service that keeps per-request state; see App::resetAfterRequest(). */
    interface Resettable
    {
        public function reset(): void;
    }
}
