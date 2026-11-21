<?php
/**
 * The fiber scheduler, one per PHP thread: it takes the events Go sends,
 * starts each request in a fiber of its own and resumes the fibers whose
 * operations are done.
 */

declare(strict_types=1);

namespace Cairo\Internal {

    use Cairo\App;
    use Cairo\Context;

    /**
     * What answers the requests of one type on a thread. A server's PHP file
     * says how to make it (Scheduler::answerWith()).
     */
    interface Answer
    {
        /**
         * Answers the request of $event, inside the request's fiber.
         *
         * @param array<string, mixed> $event
         */
        public function answer(array $event): void;

        /**
         * The route of $event's request, as logs name it.
         *
         * @param array<string, mixed> $event
         */
        public function route(array $event): string;
    }

    /** One per PHP thread: runs every request in its own fiber. */
    final class Scheduler
    {
        private static ?self $instance = null;
        /** @var array<string, \Closure(self): Answer> request type => what answerWith() was given for it */
        private static array $makers = [];
        /** @var list<\Closure(self): void> what onStart() was given */
        private static array $starters = [];

        /** @var array<string, \Closure> request type => what answers such a request on this thread */
        private array $answers = [];
        /** @var array<string, \Closure> request type => what names the route of such a request */
        private array $routes = [];
        /** @var list<\Closure(int, string): void> what afterRequest() was given, the last one first */
        private array $afterRequest = [];
        /** @var array<int, \Fiber> I/O operation id => waiting fiber */
        private array $io = [];
        private \SplQueue $ready;
        /** @internal The waits that stay on this thread: sleep(), select(). */
        public readonly Waits $waits;
        /** @var array<int, Context> request id => its context, until the request ends */
        private array $requests = [];
        /**
         * How many ready fibers, and then how many events, one turn of the
         * loop takes: fibers that keep waking each other must not keep the
         * thread from Go's events, nor from telling Go that it is not stuck.
         */
        private const TURN = 64;
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

        public function __construct(private readonly App $app)
        {
            $this->ready = new \SplQueue();
            $this->waits = new Waits($this->ready);
            $this->later = new \SplQueue();
            $this->pending = new \SplQueue();
            self::$instance = $this;
            foreach (self::$makers as $type => $make) {
                $answer = $make($this);
                $this->answers[$type] = $answer->answer(...);
                $this->routes[$type] = $answer->route(...);
            }
            foreach (self::$starters as $start) {
                $start($this);
            }
        }

        /**
         * @internal A server's PHP file says how its requests are answered.
         * Every request thread calls $make once, with its scheduler, and gets
         * the Answer for the events of that type.
         *
         * @param \Closure(self): Answer $make
         */
        public static function answerWith(string $type, \Closure $make): void
        {
            self::$makers[$type] = $make;
        }

        /**
         * @internal An extension's PHP file says what to run when a request
         * thread starts: each one calls $start once, with its scheduler,
         * before it takes its first event.
         *
         * @param \Closure(self): void $start
         */
        public static function onStart(\Closure $start): void
        {
            self::$starters[] = $start;
        }

        /**
         * @internal Calls $after when each request of this thread ends, in
         * the request's fiber once its context has finished, with the
         * request's id and its route as logs name it. What was added last is
         * called first, as destructors are.
         *
         * @param \Closure(int, string): void $after
         */
        public function afterRequest(\Closure $after): void
        {
            array_unshift($this->afterRequest, $after);
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

        /**
         * Ask Go to run $op for the current fiber, and suspend the fiber
         * until it is done. A Go operation takes its arguments as an array;
         * what the hooks ask for takes the string they built.
         *
         * @param string|array<string, mixed> $payload
         */
        public function call(string $op, string|array $payload): mixed
        {
            $ctx = Context::current();
            return $this->waitIO($ctx, \cairo_fiber_call($ctx->requestId, $ctx->scoped ? $ctx->goScope() : 0.0, $op, $payload));
        }

        /**
         * Suspend the current fiber until socket $fd is readable (or
         * writable), for $timeout seconds at most (below 0: no limit).
         * Returns 'ready' or 'timeout'.
         */
        public function wait(int $fd, bool $write, float $timeout): string
        {
            $ctx = Context::current();
            return $this->waitIO($ctx, \cairo_fiber_wait($ctx->requestId, $ctx->scoped ? $ctx->goScope() : 0.0, $fd, $write, $timeout));
        }

        /**
         * wait() for the first of several sockets.
         *
         * @param list<int> $read
         * @param list<int> $write
         */
        public function waitSet(array $read, array $write, float $timeout): string
        {
            $ctx = Context::current();
            return $this->waitIO($ctx, \cairo_fiber_waitset($ctx->requestId, $ctx->scoped ? $ctx->goScope() : 0.0, $read, $write, $timeout));
        }

        /**
         * Suspend the current fiber, which runs under $ctx, until Go reports
         * the operation that a cairo_fiber_*() call just submitted as $id.
         * Go refuses (-1) what belongs to no request it holds.
         */
        private function waitIO(Context $ctx, int $id): mixed
        {
            if ($id < 0) {
                // A fiber that its request left running: the request's context ended with it.
                $ctx->throwIfDone();
                throw new \LogicException('Cairo I/O must run inside a request handler or an async() task');
            }
            $this->io[$id] = \Fiber::getCurrent() ?? throw new \LogicException('I/O must run inside a fiber');
            if (!$ctx->scoped) {
                $event = \Fiber::suspend();
            } else {
                $ctx->goOpBegan($id);   // cancel() ends it
                $event = \Fiber::suspend();
                $ctx->goOpEnded($id);
            }
            if (isset($event['error'])) {
                if ($event['error'] === 'deadline_exceeded' || $event['error'] === 'canceled') {
                    $ctx->ended($event['error']);
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
            $ready = $this->ready;
            $waits = $this->waits;
            while (true) {
                for ($n = self::TURN; $n > 0 && !$ready->isEmpty(); $n--) {
                    $ready->dequeue()->resume();
                }
                $idle = $n > 0;   // the queue ran empty
                while (!$this->later->isEmpty()) {
                    ($this->later->dequeue())();
                    $idle = false;
                }
                // Isolated mode: start the next request once the thread is free.
                if (!$this->busy && !$this->pending->isEmpty()) {
                    $this->start($this->pending->dequeue());
                    continue;
                }
                if ($this->retiring && $this->active === 0 && $idle && $this->pending->isEmpty()) {
                    return;   // recycled: Go already runs a replacement thread
                }
                // Sleep only with no fiber to run, and no longer than until the next timer.
                $wait = -1;
                if ($waits->nextAt !== PHP_INT_MAX) {
                    $wait = $waits->tick();
                    $idle = $idle && $ready->isEmpty();
                }
                $n = self::TURN;
                $event = \cairo_next_event($idle ? $wait : 0);
                while ($event !== null) {
                    if ($event['type'] === 'io') {
                        $fiber = $this->io[$event['io']] ?? null;
                        unset($this->io[$event['io']]);
                        $fiber?->resume($event);
                    } elseif (!$this->take($event)) {
                        return;
                    }
                    // With fibers to run, the events that are there come first.
                    $event = !$idle && --$n > 0 ? \cairo_next_event(0) : null;
                }
            }
        }

        /**
         * Handle an event from Go that is not the end of an operation, which
         * the loop handles itself; false when it is the thread's last.
         *
         * @param array<string, mixed> $event
         */
        private function take(array $event): bool
        {
            switch ($event['type']) {
                case 'cancel':
                    // The request's context ended in Go before PHP answered (the
                    // client left, the deadline passed): Go ends what waits there,
                    // and this ends what waits here.
                    if (($context = $this->requests[$event['id']] ?? null) !== null) {
                        $context->ended((string) $event['error']);
                        $this->waits->end($context);
                    }
                    break;
                case 'retire':
                    // Go queues no more requests here; finish the ones held.
                    $this->retiring = true;
                    break;
                case 'shutdown':
                    return false;
                default:
                    // A request: its type names the server it came through.
                    if ($this->app->isolated() && $this->busy) {
                        $this->pending->enqueue($event);
                    } else {
                        $this->start($event);
                    }
            }
            return true;
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
            $answer = $this->answers[$event['type']];
            if (!$this->app->isolated()) {
                $this->spawn($event, $answer);
                return;
            }
            $this->busy = true;
            $this->spawn($event, function (array $event) use ($answer): void {
                try {
                    $answer($event);
                } finally {
                    // The next request must not find this one's superglobals.
                    $_GET = $_POST = $_COOKIE = $_REQUEST = $_FILES = [];
                    $_SERVER = [];
                    $this->app->runResets();
                    $this->busy = false;
                }
            });
        }

        /** Run $answer($event) in a fiber of its own, as a request with its context. */
        private function spawn(array $event, \Closure $answer): void
        {
            $id = (int) $event['id'];
            $context = new Context($id, isset($event['deadline']) ? (float) $event['deadline'] : null);
            $context->setGoValues((array) ($event['values'] ?? []));
            $this->requests[$id] = $context;
            $this->active++;
            $this->startFiber(function () use ($answer, $context, $event): void {
                try {
                    $answer($event);
                } finally {
                    $early = $context->endedAs() !== null;
                    $context->finish();
                    // The request's context ends with it, as it does in Go: what
                    // the request left waiting on this thread ends too.
                    unset($this->requests[$context->requestId]);
                    $context->ended('canceled');
                    if (Waits::$waiting !== 0) {
                        $this->waits->end($context);
                    }
                    \cairo_local_end($context->requestId);
                    if ($this->afterRequest !== []) {
                        $route = ($this->routes[$event['type']])($event);
                        foreach ($this->afterRequest as $after) {
                            $after($context->requestId, $route);
                        }
                    }
                    $this->active--;
                    $this->served++;
                    if ($early) {
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

        /** @internal The handler registered as $id; a class given by name becomes an object, once per thread. */
        public function resolve(int $id): mixed
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
    }
}
