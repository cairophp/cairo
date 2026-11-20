<?php

// Cairo\Pool: a per-thread pool of connections. Loaded by runtime.php's
// autoloader on first use.

declare(strict_types=1);

namespace Cairo {

    use Cairo\Internal\PoolWaiter;
    use Cairo\Internal\Scheduler;
    use Cairo\Internal\Waiter;

    /** Cairo\Pool or Cairo\Sql\Db had no free connection within its acquire timeout. */
    final class PoolTimeout extends \RuntimeException
    {
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
     * fiber until one is returned. A request past its deadline is skipped
     * when a connection comes back (a Cairo\Context\ContextException). With
     * $acquireTimeout the wait also ends after that many seconds (a
     * PoolTimeout) and the moment the request is cancelled; that wait goes
     * through Go, which costs a little on a pool that is always full.
     *
     * A connection is reset before the next fiber gets it: by default a PDO
     * transaction left open is rolled back; pass $reset to do more, such as
     * `fn (PDO $pdo) => $pdo->exec('DISCARD ALL')`. A connection whose reset
     * throws is dropped. $maxLifetime and $maxIdleTime (seconds, 0: no limit)
     * retire connections before the server or a firewall cuts them.
     */
    final class Pool
    {
        /** @var list<array{mixed, float, float}> connection, opened at, idle since */
        private array $idle = [];
        private int $open = 0;
        /** @var \WeakMap<Context, array{mixed, float, float}> fiber context => its connection */
        private \WeakMap $held;
        /** @var array<int, PoolWaiter> fibers waiting for a connection, oldest first */
        private array $waiters = [];
        /** @var \Closure(): mixed */
        private \Closure $factory;
        /** @var (\Closure(mixed): void)|null */
        private ?\Closure $reset;
        private bool $ages;

        /** @param (callable(mixed): void)|null $reset */
        public function __construct(
            callable $factory,
            private readonly int $max = 16,
            private readonly ?float $acquireTimeout = null,
            private readonly float $maxLifetime = 0,
            private readonly float $maxIdleTime = 0,
            ?callable $reset = null,
        ) {
            if ($max < 1) {
                throw new \InvalidArgumentException('a pool needs max >= 1');
            }
            if (($acquireTimeout ?? 0) < 0 || $maxLifetime < 0 || $maxIdleTime < 0) {
                throw new \InvalidArgumentException('a pool\'s timeouts and lifetimes cannot be negative');
            }
            $this->factory = \Closure::fromCallable($factory);
            $this->reset = $reset === null ? null : \Closure::fromCallable($reset);
            $this->ages = $maxLifetime > 0 || $maxIdleTime > 0;
            $this->held = new \WeakMap();
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
                return $this->held[$ctx][0];
            }
            $entry = $this->acquire();
            $this->held[$ctx] = $entry;
            Context::defer(fn () => $this->release($ctx));
            return $entry[0];
        }

        /**
         * Borrow a connection for $fn only, then return it. When $fn throws,
         * or the request was cancelled meanwhile, the connection is dropped
         * instead: it may have been cut off in the middle of a reply.
         */
        public function with(callable $fn): mixed
        {
            $entry = $this->acquire();
            $ok = false;
            try {
                $result = $fn($entry[0]);
                $ok = true;
                return $result;
            } finally {
                if ($ok && Context::current()->err() === null) {
                    $this->giveBack($entry);
                } else {
                    $this->drop();
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
                $this->drop();
            }
        }

        /** @return array{open: int, idle: int, waiting: int, max: int} */
        public function stats(): array
        {
            return ['open' => $this->open, 'idle' => count($this->idle), 'waiting' => count($this->waiters), 'max' => $this->max];
        }

        /** @return array{mixed, float, float} */
        private function acquire(): array
        {
            while (true) {
                $now = $this->ages ? microtime(true) : 0.0;
                while ($this->idle !== []) {
                    $entry = array_pop($this->idle);
                    if (!$this->ages || !$this->expired($entry, $now)) {
                        return $entry;
                    }
                    $this->open--;
                }
                if ($this->open < $this->max) {
                    $this->open++;
                    try {
                        return [($this->factory)(), $now, $now];
                    } catch (\Throwable $e) {
                        $this->drop();
                        throw $e;
                    }
                }
                if (($entry = $this->wait()) !== null) {
                    return $entry;
                }
                // A connection was dropped while this fiber waited: open one.
            }
        }

        /**
         * Wait for a returned connection, or null when a slot was freed.
         *
         * Without an acquireTimeout the fiber waits in PHP alone, which costs
         * nothing but only ends when the pool has something for it. With one
         * it parks through Go, which also ends the wait at the timeout and
         * the moment the request is cancelled.
         *
         * @return array{mixed, float, float}|null
         */
        private function wait(): ?array
        {
            $ctx = Context::current();
            $fiber = \Fiber::getCurrent();
            if ($fiber === null || $ctx->fiberId === 0 || !Scheduler::running()) {
                throw new \LogicException('the pool is exhausted and there is no request fiber to wait in');
            }
            $waiter = new PoolWaiter($fiber, $ctx, $this->acquireTimeout === null ? null : new Waiter());
            $id = spl_object_id($waiter);
            $this->waiters[$id] = $waiter;
            $ended = null;
            try {
                if ($waiter->park === null) {
                    \Fiber::suspend();
                } else {
                    $ended = Scheduler::instance()->waits->park($ctx, $this->acquireTimeout, $waiter->park);
                }
            } finally {
                unset($this->waiters[$id]);
            }
            if ($waiter->entry !== null || $waiter->slot) {
                return $waiter->entry;
            }
            if ($ended !== null) {
                $ctx->ended($ended);
            }
            if (($err = $ctx->err()) !== null) {
                throw $err;
            }
            throw new PoolTimeout(sprintf(
                'no connection became free in %.3g s: all %d are in use and %d more requests are waiting',
                $this->acquireTimeout,
                $this->max,
                count($this->waiters),
            ));
        }

        /** @param array{mixed, float, float} $entry */
        private function expired(array $entry, float $now): bool
        {
            return ($this->maxLifetime > 0 && $now - $entry[1] >= $this->maxLifetime)
                || ($this->maxIdleTime > 0 && $now - $entry[2] >= $this->maxIdleTime);
        }

        private function release(Context $ctx): void
        {
            if (!isset($this->held[$ctx])) {
                return;
            }
            $entry = $this->held[$ctx];
            unset($this->held[$ctx]);
            if ($ctx->err() !== null) {
                // Cancelled or past its deadline: an I/O wait on the
                // connection may have been cut off, so it is not reused.
                $this->drop();
                return;
            }
            $this->giveBack($entry);
        }

        /** @param array{mixed, float, float} $entry */
        private function giveBack(array $entry): void
        {
            try {
                if ($this->reset !== null) {
                    ($this->reset)($entry[0]);
                } elseif ($entry[0] instanceof \PDO && $entry[0]->inTransaction()) {
                    $entry[0]->rollBack();
                }
            } catch (\Throwable $e) {
                error_log('cairo: pool: dropped a connection whose reset failed: ' . $e->getMessage());
                $this->drop();
                return;
            }
            if ($this->ages) {
                $entry[2] = microtime(true);
                if ($this->expired($entry, $entry[2])) {
                    $this->drop();
                    return;
                }
            }
            if (($waiter = $this->nextWaiter()) !== null) {
                $waiter->entry = $entry;
                $waiter->wake();
                return;
            }
            $this->idle[] = $entry;
        }

        /** A connection is gone: let the longest waiting fiber open a new one. */
        private function drop(): void
        {
            $this->open--;
            if (($waiter = $this->nextWaiter()) !== null) {
                $waiter->slot = true;
                $waiter->wake();
            }
        }

        /** The longest waiting fiber whose request is still alive. */
        private function nextWaiter(): ?PoolWaiter
        {
            foreach ($this->waiters as $id => $waiter) {
                unset($this->waiters[$id]);
                if ($waiter->ctx->err() === null) {
                    return $waiter;
                }
                $waiter->wake();   // past its deadline: it wakes with nothing and throws
            }
            return null;
        }
    }
}

namespace Cairo\Internal {

    use Cairo\Context;

    /** A fiber waiting for one of a Cairo\Pool's connections. */
    final class PoolWaiter
    {
        /** @var array{mixed, float, float}|null the connection it was handed */
        public ?array $entry = null;
        /** A connection was dropped: it may open a new one. */
        public bool $slot = false;

        public function __construct(
            public readonly \Fiber $fiber,
            public readonly Context $ctx,
            public readonly ?Waiter $park,
        ) {
        }

        public function wake(): void
        {
            if ($this->park === null) {
                Scheduler::instance()->ready($this->fiber);
            } else {
                $this->park->wake(0, null, true);
            }
        }
    }
}
