<?php

// Cairo\Pool: a per-thread pool of connections. Loaded by runtime.php's
// autoloader on first use.

declare(strict_types=1);

namespace Cairo {

    use Cairo\Internal\Scheduler;

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
         * fiber, returned to the pool when the fiber ends (dropped if the
         * request was cancelled; call discard() after a connection error). A
         * child task from async() gets its own.
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

        /**
         * Borrow a connection for $fn only, then return it. When $fn throws,
         * or the request was cancelled meanwhile, the connection is dropped
         * instead: it may have been cut off in the middle of a reply.
         */
        public function with(callable $fn): mixed
        {
            $conn = $this->acquire();
            $ok = false;
            try {
                $result = $fn($conn);
                $ok = true;
                return $result;
            } finally {
                if ($ok && Context::current()->err() === null) {
                    $this->giveBack($conn);
                } else {
                    $this->open--;
                    $this->wakeOne();
                }
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
            if ($ctx->err() !== null) {
                // Cancelled or past its deadline: an I/O wait on the
                // connection may have been cut off, so it is not reused.
                $this->open--;
                $this->wakeOne();
                return;
            }
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
}
