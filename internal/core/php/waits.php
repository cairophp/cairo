<?php
/**
 * The waits that never leave the PHP thread: a fiber in sleep(), blocked in
 * select() or waiting on a socket parks here and runs again when its time is
 * up, another fiber wakes it, its socket is ready, or its context ends.
 */

declare(strict_types=1);

namespace Cairo\Internal {

    use Cairo\Context;

    /** The parked fibers of one PHP thread and their timers. */
    final class Waits
    {
        /** What a fiber waits in, as cairo_wait_ended() tells it to a probe (threadWaits in probe.go). */
        public const SLEEP = 0;
        public const PARKED = 1;
        public const SOCKET = 2;
        public const SOCKETS = 3;
        public const OFFLOAD = 4;

        private int $lastId = 0;

        /**
         * Park the current fiber for at most $seconds (below 0: no limit),
         * until wake() with the id that $waiter got in `op`, or until $ctx
         * ends. Returns how $ctx ended ('canceled' or 'deadline_exceeded'),
         * or null: the time was up, or the waiter was woken and says so.
         * $kind is one of the constants above. The thread keeps the fiber
         * and its timer in C (cairo_park() in parked.c).
         */
        public function park(Context $ctx, float $seconds, ?Waiter $waiter = null, int $kind = self::PARKED): ?string
        {
            if ($ctx->requestId === 0) {
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
            \cairo_park($id, $ctx, $ctx->requestId, $at, $kind);
            return $ctx->endedAs();
        }

        /** Cairo\sleep() and the hooked sleep functions: throws when the fiber's context ends first. */
        public function sleep(float $seconds): void
        {
            $ctx = Context::current();
            if (($ended = $this->park($ctx, max(0.0, $seconds), null, self::SLEEP)) !== null) {
                throw self::exceptionOf($ctx, $ended);
            }
        }

        /**
         * Suspend the current fiber until socket $fd is readable (or
         * writable), for $seconds at most (below 0: no limit). Returns
         * whether it is; false when the time was up. Throws when $ctx ends
         * first. The thread's loop watches the socket and wakes the fiber
         * (cairo_wait_socket() in sockets.c); one fiber waits on a socket at
         * a time.
         */
        public function socket(Context $ctx, int $fd, bool $write, float $seconds): bool
        {
            $at = self::until($ctx, $seconds);
            $ready = \cairo_wait_socket($fd, $write ? 2 : 1, ++$this->lastId, $ctx, $ctx->requestId, $at);
            if (!$ready && ($ended = $ctx->endedAs()) !== null) {
                throw self::exceptionOf($ctx, $ended);
            }
            return $ready;
        }

        /**
         * socket() for the first of several sockets, as curl waits: any of
         * $read readable or any of $write writable. None at all waits out
         * the time.
         *
         * @param list<int> $read
         * @param list<int> $write
         */
        public function sockets(Context $ctx, array $read, array $write, float $seconds): void
        {
            $events = array_fill_keys($read, 1);
            foreach ($write as $fd) {
                $events[$fd] = ($events[$fd] ?? 0) | 2;
            }
            $at = self::until($ctx, $seconds);
            $ready = \cairo_wait_sockets($events, ++$this->lastId, $ctx, $ctx->requestId, $at);
            if (!$ready && ($ended = $ctx->endedAs()) !== null) {
                throw self::exceptionOf($ctx, $ended);
            }
        }

        /**
         * A wait on sockets begins under $ctx, for $seconds at most: the
         * tick of hrtime()'s clock at which its time is up. Throws when $ctx
         * has ended.
         */
        private static function until(Context $ctx, float $seconds): int
        {
            if ($ctx->requestId === 0) {
                throw new \LogicException('Cairo I/O must run inside a request handler or an async() task');
            }
            if (($ended = $ctx->endedAs()) !== null) {
                throw self::exceptionOf($ctx, $ended);
            }
            $at = $ctx->deadlineTick();
            // A limit of thirty years or more is none: it would not fit the clock.
            if ($seconds >= 0 && $seconds < 1e9 && ($limit = hrtime(true) + (int) ($seconds * 1e9)) < $at) {
                $at = $limit;
            }
            return $at;
        }

        /** @internal The exception of $ctx, which ended as $ended while a fiber waited under it. */
        public static function exceptionOf(Context $ctx, string $ended): \Throwable
        {
            $ctx->ended($ended);
            return $ended === 'canceled' ? new Context\Canceled() : new Context\DeadlineExceeded();
        }

        /** End wait $id if it still goes on: its fiber runs at the loop's next turn. */
        public function wake(int $id): void
        {
            \cairo_wake($id);
        }

        /**
         * $ctx ended, and whoever calls has marked how: wake the fibers that
         * wait under it or under a context that derives from it.
         */
        public function end(Context $ctx): void
        {
            foreach (\cairo_parked_under($ctx->requestId) as $id => $waiting) {
                if ($waiting->within($ctx)) {
                    \cairo_wake($id);
                }
            }
        }
    }
}
