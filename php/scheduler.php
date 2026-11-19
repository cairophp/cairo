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

    /** One per PHP thread: runs every request in its own fiber. */
    final class Scheduler
    {
        private static ?self $instance = null;
        /** @var array<string, \Closure(self): \Closure> request type => what answerWith() was given for it */
        private static array $makers = [];

        /** @var array<string, \Closure> request type => what answers such a request on this thread */
        private array $answers = [];
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
        /** App::detectLeaks() is on */
        private readonly bool $watchLeaks;

        public function __construct(private readonly App $app)
        {
            $this->ready = new \SplQueue();
            $this->later = new \SplQueue();
            $this->pending = new \SplQueue();
            self::$instance = $this;
            foreach (self::$makers as $type => $make) {
                $this->answers[$type] = $make($this);
            }
            self::$watchQueries = $app->repeatedQueriesAt() > 0;
            \cairo_watch_queries(self::$watchQueries);
            $ignored = $app->leaksIgnored();
            $this->watchLeaks = $ignored !== null;
            if ($ignored !== null) {
                Leaks::start($ignored);
            }
        }

        /**
         * @internal A server's PHP file says how its requests are answered.
         * Every request thread calls $make once, with its scheduler, and gets
         * what it will call with each event of that type, inside the request's
         * fiber.
         *
         * @param \Closure(self): \Closure $make
         */
        public static function answerWith(string $type, \Closure $make): void
        {
            self::$makers[$type] = $make;
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
                    case 'retire':
                        // Go queues no more requests here; finish the ones held.
                        $this->retiring = true;
                        break;
                    case 'shutdown':
                        return;
                    default:
                        // A request: its type names the server it came through.
                        if ($this->app->isolated() && $this->busy) {
                            $this->pending->enqueue($event);
                        } else {
                            $this->start($event);
                        }
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
            $id = (int) $event['id'];   // the request's root fiber is served by the request goroutine
            $context = new Context($id, $id, isset($event['deadline']) ? (float) $event['deadline'] : null);
            $context->setGoValues((array) ($event['values'] ?? []));
            $this->active++;
            $this->startFiber(function () use ($answer, $context, $event): void {
                try {
                    $answer($event);
                } finally {
                    $context->finish();
                    \cairo_local_end($context->requestId);
                    if (self::$watchQueries) {
                        Queries::end($context->requestId, $this->routeOf($event), $this->app->repeatedQueriesAt());
                    }
                    if ($this->watchLeaks) {
                        Leaks::check($this->routeOf($event));
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
