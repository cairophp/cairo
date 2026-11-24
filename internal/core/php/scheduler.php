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
        /** How many of those are fibers that a script does not wait for (callAside()). */
        private int $aside = 0;
        /** @internal The waits that stay on this thread: sleep(), select(), sockets. */
        public readonly Waits $waits;
        /** @internal The offload() jobs that fibers of this thread wait for. */
        public readonly Jobs $jobs;
        /** @var array<int, Context> request id => its context, until the request ends */
        private array $requests = [];
        /**
         * How many ready fibers, and then how many events, one turn of the
         * loop takes: fibers that keep waking each other must not keep the
         * thread from Go's events, nor from telling Go that it is not stuck.
         */
        private const TURN = 64;
        /** The stack of the fiber that script() runs: what cairo_cli() in cli.c gives the thread. */
        private const SCRIPT_STACK = 16 << 20;
        /** @var \SplQueue<\Closure> work to start from this loop, outside any fiber */
        private \SplQueue $later;
        /** @var array<int, mixed> handler id => resolved handler */
        private array $resolved = [];

        /** Isolated mode: requests waiting for the one in progress to finish. */
        private readonly bool $isolated;
        private \SplQueue $pending;
        private bool $busy = false;

        /** Thread recycling (App::maxRequests(), App::maxMemory(); 0 is no limit). */
        private readonly int $maxRequests;
        private readonly int $maxMemory;
        private int $active = 0;
        private int $served = 0;

        /** What a fiber of startFiber() runs: its jobs, one after the other. */
        private readonly \Closure $work;
        /** A task failed or a request ended early since the last collectAfterFailure(). */
        private bool $failed = false;
        private float $lastCollect = 0.0;
        private bool $retireAsked = false;
        /** No request comes any more: the thread leaves once it holds nothing. */
        private bool $leaving = false;
        /** @internal The thread runs the script of the command line (script()): it has no offload threads. */
        public bool $script = false;
        /** The script's fiber, for as long as the script runs: PHP destroys a suspended fiber that nothing holds. */
        private ?\Fiber $main = null;

        public function __construct(private readonly App $app)
        {
            $this->waits = new Waits();
            $this->work = function (\Closure|array $job): void {
                do {
                    $job instanceof \Closure ? $job() : $this->serve($job);
                    unset($job);   // an idle fiber must not keep the last job's captures alive
                } while (($job = \cairo_fiber_idle()) !== null);
            };
            $this->jobs = new Jobs($this->waits);
            $this->later = new \SplQueue();
            $this->pending = new \SplQueue();
            $this->isolated = $app->isolated();
            [$this->maxRequests, $this->maxMemory] = $app->recycleLimits();
            self::$instance = $this;
            foreach (self::$makers as $type => $make) {
                $answer = $make($this);
                $this->answers[$type] = $this->isolated ? $this->alone($answer->answer(...)) : $answer->answer(...);
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
         * Run $job in a fiber bound to $context: a closure, or the event of
         * a request to serve. A fiber whose job is done waits for the next one
         * (cairo_fiber_idle() in fibers.c), and a new fiber is made only when
         * none waits.
         *
         * @param \Closure|array<string, mixed> $job
         */
        public function startFiber(\Closure|array $job, Context $context): void
        {
            if (!\cairo_fiber_reuse($context, $context->requestId, $job)) {
                $fiber = new \Fiber($this->work);
                Context::bind($fiber, $context);
                $fiber->start($job);
            }
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
         * @internal call() for a fiber that watches for what may never come,
         * as the one behind a script's signal handlers does: a script with
         * one fiber to run still waits with its thread (waitsAlone()), and
         * this fiber learns of the end of $op once the script waits as
         * fibers do. Who needs it sooner wakes the thread out of its wait
         * itself.
         *
         * @param array<string, mixed> $payload
         */
        public function callAside(string $op, array $payload): mixed
        {
            $this->aside++;
            try {
                return $this->call($op, $payload);
            } finally {
                $this->aside--;
            }
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

        /** $fiber, which is suspended, runs again at the loop's next turn. */
        public function ready(\Fiber $fiber): void
        {
            \cairo_ready($fiber);
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
                $idle = \cairo_run_ready(self::TURN);   // whether the queue ran empty
                while (!$this->later->isEmpty()) {
                    ($this->later->dequeue())();
                    $idle = false;
                }
                // Isolated mode: start the next request once the thread is free.
                if (!$this->busy && !$this->pending->isEmpty()) {
                    $this->start($this->pending->dequeue());
                    continue;
                }
                // A fiber that still waits for Go counts too: PHP would destroy
                // it in the middle of its request when the thread ends. A script
                // that nothing can wake any more would wait for ever.
                if ($this->leaving && $this->io === [] && $idle && $this->pending->isEmpty()
                    && ($this->active === 0 || ($this->script && \cairo_stuck()))) {
                    return;
                }
                // The thread sleeps there only when this is idle and no fiber is
                // ready, and no longer than until the next timer of a parked one.
                $n = self::TURN;
                $event = \cairo_next_event($idle);
                while ($event !== null) {
                    if (($type = $event['type']) === 'io') {
                        $fiber = $this->io[$event['io']] ?? null;
                        unset($this->io[$event['io']]);
                        $fiber?->resume($event);
                    } elseif (!isset($this->answers[$type])) {
                        $this->take($event);
                    } elseif ($this->busy) {
                        $this->pending->enqueue($event);   // isolated mode: one request at a time
                    } else {
                        $this->start($event);
                    }
                    // With fibers to run, the events that are there come first;
                    // which sockets are ready is asked once a turn.
                    $event = !$idle && --$n > 0 ? \cairo_next_event(false, false) : null;
                }
            }
        }

        /**
         * @internal Runs $script, the script of the command line, as the one
         * request this thread ever takes: in a fiber of its own, under a
         * context of request $id, which Go holds a call for. The loop runs
         * until the script and the fibers it started have ended; what runs
         * after it (shutdown functions, destructors) finds no scheduler, and
         * blocks as it does under PHP. Returns false when the script still
         * waits and nothing is left that could wake it.
         */
        public function script(int $id, \Closure $script): bool
        {
            $this->script = $this->leaving = true;
            $this->answers['script'] = function () use ($script): void {
                ini_restore('fiber.stack_size');   // the fibers that the script starts are fibers
                $this->main = \Fiber::getCurrent();
                try {
                    $script();
                } finally {
                    $this->main = null;
                }
            };
            $this->routes['script'] = static fn (): string => 'script';
            // The script gets a stack as large as its thread's, as it has
            // under PHP, and not the 2 MiB of a fiber.
            ini_set('fiber.stack_size', (string) self::SCRIPT_STACK);
            $this->start(['type' => 'script', 'id' => $id]);
            $this->run();
            self::$instance = null;
            return $this->active === 0;
        }

        /**
         * Handle an event from Go that is neither a request nor the end of
         * an operation, which the loop handles itself.
         *
         * @param array<string, mixed> $event
         */
        private function take(array $event): void
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
                case 'offloaded':
                    $this->jobs->done($event);
                    break;
                case 'retire':
                case 'shutdown':
                    // Go queues no more requests here: a replacement thread
                    // takes them, or the servers have stopped. The ones held
                    // are finished first, also those whose client has left.
                    $this->leaving = true;
                    break;
                default:
                    throw new \LogicException("cairo: no server answers '{$event['type']}' events");
            }
        }

        public function app(): App
        {
            return $this->app;
        }

        /** After a request: ask Go for a fresh thread once a recycle limit is reached. */
        private function checkRecycle(): void
        {
            $reason = match (true) {
                $this->maxRequests > 0 && $this->served >= $this->maxRequests => 'max_requests',
                $this->maxMemory > 0 && memory_get_usage() >= $this->maxMemory => 'max_memory',
                default => null,
            };
            if ($reason !== null) {
                $this->retireAsked = true;
                \cairo_retire($reason);
            }
        }

        /**
         * Isolated mode: $answer for a thread that serves one request at a
         * time, and leaves nothing of it for the next.
         */
        private function alone(\Closure $answer): \Closure
        {
            return function (array $event) use ($answer): void {
                try {
                    $answer($event);
                } finally {
                    // The next request must not find this one's superglobals.
                    $_GET = $_POST = $_COOKIE = $_REQUEST = $_FILES = [];
                    $_SERVER = [];
                    $this->app->runResets();
                    $this->busy = false;
                }
            };
        }

        /**
         * Start the request of $event in a fiber of its own, with its context.
         *
         * @param array<string, mixed> $event
         */
        private function start(array $event): void
        {
            $id = (int) $event['id'];
            $context = new Context($id, isset($event['deadline']) ? (float) $event['deadline'] : null);
            if (isset($event['values'])) {
                $context->setGoValues((array) $event['values']);
            }
            $this->requests[$id] = $context;
            $this->active++;
            $this->busy = $this->isolated;
            $this->startFiber($event, $context);
        }

        /**
         * Answer the request of $event, in its fiber, and end what it leaves
         * behind.
         *
         * @param array<string, mixed> $event
         */
        private function serve(array $event): void
        {
            $id = (int) $event['id'];
            $context = $this->requests[$id];
            try {
                ($this->answers[$event['type']])($event);
            } finally {
                $early = $context->endedAs() !== null;
                $context->finish();
                // The request's context ends with it, as it does in Go: what
                // the request left waiting on this thread ends too.
                unset($this->requests[$id]);
                $context->ended('canceled');
                if (\cairo_parked() !== 0) {
                    $this->waits->end($context);
                }
                \cairo_local_end($id);
                if ($this->afterRequest !== []) {
                    $route = ($this->routes[$event['type']])($event);
                    foreach ($this->afterRequest as $after) {
                        $after($id, $route);
                    }
                }
                $this->active--;
                $this->served++;
                if ($early || $this->failed) {
                    $this->failed = true;
                    $this->collectAfterFailure();
                }
                if (!$this->retireAsked && ($this->maxRequests > 0 || $this->maxMemory > 0)) {
                    $this->checkRecycle();
                }
            }
        }

        /**
         * @internal Wait for socket $fd with the whole thread for a moment
         * (cairo_wait_here() in sockets.c), where it could run nothing else
         * meanwhile: it serves one request at a time, and no other fiber of
         * that request is ready, waiting for Go or parked on the thread. A
         * thread that runs requests side by side never does, because the
         * next request would wait with it. Returns whether the socket became
         * ready.
         */
        public function waitHere(int $fd, bool $write): bool
        {
            return $this->busy && $this->io === [] && \cairo_parked() === 0 && \cairo_run_ready(0)
                && $this->later->isEmpty() && \cairo_wait_here($fd, $write);
        }

        /**
         * @internal Whether the current fiber, which runs under $ctx, waits
         * with the whole thread: it is a fiber of a script, the thread could
         * run nothing else meanwhile (no fiber is ready or waits for this
         * one to suspend, none waits for Go but aside, none is parked on the
         * thread), and
         * nothing can end $ctx while it waits. So a script that runs one
         * fiber at a time waits as it does under PHP, at PHP's cost, and its
         * waits suspend fibers once it has several.
         */
        public function waitsAlone(Context $ctx): bool
        {
            return $this->script && !$ctx->scoped && \count($this->io) === $this->aside && \cairo_fiber_alone() && \cairo_parked() === 0
                && $this->later->isEmpty() && $ctx->endedAs() === null;
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
