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
        private int $hooks = Hook::ALL;
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
                $task->result = $fn();
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
     * Runtime hooks: blocking PHP functions that suspend the current fiber
     * instead of blocking the whole thread. See App::hooks().
     */
    final class Hook
    {
        public const NONE = 0;
        /** sleep(), usleep(), time_nanosleep() */
        public const SLEEP = 1;
        public const ALL = self::SLEEP;
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
            ];
        }
    }

    final class Request
    {
        /** @internal */
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

namespace Cairo\Grpc {

    use Cairo\App;

    final class Server
    {
        /** @var list<string> */
        private array $protos = [];
        /** @var list<array{id:int,name:string}> */
        private array $services = [];
        private float $maxDuration = 0;

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

        public function __construct(private readonly App $app)
        {
            $this->ready = new \SplQueue();
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
                while (!$this->ready->isEmpty()) {
                    $this->ready->dequeue()->resume();
                }
                $event = json_decode(\cairo_next_event(), true, 512, JSON_THROW_ON_ERROR);
                switch ($event['type']) {
                    case 'io':
                        $fiber = $this->io[$event['io']] ?? null;
                        unset($this->io[$event['io']]);
                        $fiber?->resume($event);
                        break;
                    case 'http':
                        $this->spawn($event, fn () => $this->http($event));
                        break;
                    case 'grpc':
                        $this->spawn($event, fn () => $this->grpc($event));
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

        private function spawn(array $event, \Closure $fn): void
        {
            $id = (int) $event['id'];   // the request's root fiber is served by the request goroutine
            $context = new Context($id, $id, isset($event['deadline']) ? (float) $event['deadline'] : null);
            $fiber = new \Fiber(static function () use ($fn, $context): void {
                try {
                    $fn();
                } finally {
                    $context->finish();
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
            try {
                $response = ($this->resolve((int) $event['route']))(new Http\Request($event), Context::current());
                if (!$response instanceof Http\Response) {
                    $response = is_string($response) ? Http\Response::text($response) : Http\Response::json($response);
                }
            } catch (Context\DeadlineExceeded) {
                $response = Http\Response::text("Gateway Timeout\n", 504);
            } catch (\Throwable $e) {
                error_log('cairo: ' . $e);
                $response = Http\Response::text("Internal Server Error\n", 500);
            }
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

        private function grpc(array $event): void
        {
            try {
                $service = $this->resolve((int) $event['service']);
                $method = (string) $event['method'];
                if (!is_object($service) || !method_exists($service, $method)) {
                    throw new Grpc\Exception(Grpc\Code::UNIMPLEMENTED, "method $method is not implemented");
                }
                $result = $service->{$method}($event['message'] ?? [], Context::current());
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
            \cairo_respond((int) $event['id'], json_encode($out, JSON_THROW_ON_ERROR | JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE));
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
