<?php
/**
 * Cairo\Channel and select(): how the fibers of one PHP thread hand
 * values to one another and wait for one another.
 */

declare(strict_types=1);

namespace Cairo {

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

namespace Cairo\Internal {

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
}
