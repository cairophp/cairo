<?php
/**
 * The waits that never leave the PHP thread: a fiber in sleep() or blocked
 * in select() parks here and runs again when its time is up, another fiber
 * wakes it, or its context ends.
 */

declare(strict_types=1);

namespace Cairo\Internal {

    use Cairo\Context;

    /** The parked fibers of one PHP thread and their timers. */
    final class Waits
    {
        /** @internal How many fibers are parked on this thread: with none, the end of a context has nobody to wake here. */
        public static int $waiting = 0;

        /** @var array<int, \Fiber> wait id => the fiber parked in it */
        private array $parked = [];
        /** @var array<int, array<int, Context>> request id => wait id => the context the wait is under */
        private array $under = [];
        private int $lastId = 0;

        /** @var \SplPriorityQueue<int, int> the ids of the waits with a limit; the priority is minus its time, so the first one due is on top */
        private \SplPriorityQueue $timers;
        /** @internal When the first timer is due, on hrtime()'s clock; PHP_INT_MAX with none, and then the loop does not call tick(). */
        public int $nextAt = PHP_INT_MAX;
        /** How many parked fibers have a timer: the other entries of $timers are of waits that ended early. */
        private int $timed = 0;

        /** @param \SplQueue<\Fiber> $ready the scheduler's queue of fibers to resume */
        public function __construct(private readonly \SplQueue $ready)
        {
            $this->timers = new \SplPriorityQueue();
        }

        /**
         * Park the current fiber for at most $seconds (below 0: no limit),
         * until wake() with the id that $waiter got in `op`, or until $ctx
         * ends. Returns how $ctx ended ('canceled' or 'deadline_exceeded'),
         * or null: the time was up, or the waiter was woken and says so.
         */
        public function park(Context $ctx, float $seconds, ?Waiter $waiter = null): ?string
        {
            if ($ctx->fiberId === 0) {
                throw new \LogicException('Cairo I/O must run inside a request handler or an async() task');
            }
            if (($ended = $ctx->endedAs()) !== null) {
                return $ended;
            }
            $at = $ctx->deadlineTick();
            // A limit of thirty years or more is none: it would not fit the clock.
            if ($seconds >= 0 && $seconds < 1e9 && ($limit = hrtime(true) + (int) ($seconds * 1e9)) < $at) {
                $at = $limit;
            }
            $id = ++$this->lastId;
            if ($waiter !== null) {
                $waiter->op = $id;
            }
            $this->parked[$id] = \Fiber::getCurrent();
            $this->under[$ctx->requestId][$id] = $ctx;
            if ($at !== PHP_INT_MAX) {
                $this->setTimer($id, $at);
            }
            $began = \cairo_wait_began($ctx->fiberId);
            self::$waiting++;
            \Fiber::suspend();
            self::$waiting--;
            if ($began !== 0) {
                \cairo_wait_ended($ctx->fiberId, $waiter !== null, $began);
            }
            if ($at !== PHP_INT_MAX) {
                $this->timed--;
            }
            unset($this->under[$ctx->requestId][$id]);
            if (($this->under[$ctx->requestId] ?? null) === []) {
                unset($this->under[$ctx->requestId]);
            }
            return $ctx->endedAs();
        }

        /** Cairo\sleep() and the hooked sleep functions: throws when the fiber's context ends first. */
        public function sleep(float $seconds): void
        {
            $ctx = Context::current();
            if (($ended = $this->park($ctx, max(0.0, $seconds))) !== null) {
                $ctx->ended($ended);
                throw $ended === 'canceled' ? new Context\Canceled() : new Context\DeadlineExceeded();
            }
        }

        /** End wait $id if it still goes on: its fiber runs at the loop's next turn. */
        public function wake(int $id): void
        {
            if (($fiber = $this->parked[$id] ?? null) !== null) {
                unset($this->parked[$id]);
                $this->ready->enqueue($fiber);
            }
        }

        /**
         * $ctx ended, and whoever calls has marked how: wake the fibers that
         * wait under it or under a context that derives from it.
         */
        public function end(Context $ctx): void
        {
            foreach ($this->under[$ctx->requestId] ?? [] as $id => $waiting) {
                if ($waiting->within($ctx)) {
                    $this->wake($id);
                }
            }
        }

        /**
         * Wake the fibers whose time is up. Returns how long the thread may
         * sleep before the next one is due, in nanoseconds; below 0 when
         * no timer is set.
         */
        public function tick(): int
        {
            if ($this->nextAt === PHP_INT_MAX) {
                return -1;
            }
            $now = hrtime(true);
            while ($this->nextAt <= $now) {
                $this->wake($this->timers->extract());
                $this->nextAt = $this->firstDue();
            }
            return $this->nextAt === PHP_INT_MAX ? -1 : $this->nextAt - $now;
        }

        private function firstDue(): int
        {
            if ($this->timers->isEmpty()) {
                return PHP_INT_MAX;
            }
            $this->timers->setExtractFlags(\SplPriorityQueue::EXTR_PRIORITY);
            $at = -$this->timers->top();
            $this->timers->setExtractFlags(\SplPriorityQueue::EXTR_DATA);
            return $at;
        }

        private function setTimer(int $id, int $at): void
        {
            $this->timed++;
            // A wait that ended early leaves its timer behind until it is
            // due: drop those once they outnumber the timers still awaited.
            if (count($this->timers) > 2 * $this->timed + 64) {
                $this->timers->setExtractFlags(\SplPriorityQueue::EXTR_BOTH);
                $live = new \SplPriorityQueue();
                foreach ($this->timers as $timer) {
                    if (isset($this->parked[$timer['data']])) {
                        $live->insert($timer['data'], $timer['priority']);
                    }
                }
                $this->timers = $live;
                $this->nextAt = $this->firstDue();
            }
            $this->timers->insert($id, -$at);
            if ($at < $this->nextAt) {
                $this->nextAt = $at;
            }
        }
    }
}
