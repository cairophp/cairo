<?php
/**
 * Cairo\Context: the Go context.Context of the current fiber as PHP sees
 * it, with its deadline, its cancellation and its values.
 */

declare(strict_types=1);

namespace Cairo {

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
