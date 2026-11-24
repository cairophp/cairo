<?php
/**
 * Cairo\Internal\Go: how PHP asks Go to run one of its registered
 * operations (see internal/core/goops.go).
 */

declare(strict_types=1);

namespace Cairo\Internal {

    use Cairo\Context;

    /**
     * Runs a Go operation (see internal/core/goops.go): in a fiber of a
     * request the fiber suspends while a goroutine runs it; elsewhere, and
     * in the only fiber a script has to run (Scheduler::waitsAlone()), the
     * thread waits for Go.
     */
    final class Go
    {
        /** @param array<string, mixed> $payload */
        public static function call(string $op, array $payload): mixed
        {
            if (Scheduler::running() && ($ctx = Context::current())->requestId !== 0) {
                $scheduler = Scheduler::instance();
                if (!$scheduler->script || !$scheduler->waitsAlone($ctx)) {
                    return $scheduler->call($op, $payload);
                }
            }
            return self::callNow($op, $payload);
        }

        /**
         * Run $op on this thread without suspending the fiber: only for ops
         * that never wait.
         *
         * @param array<string, mixed> $payload
         */
        public static function callNow(string $op, array $payload): mixed
        {
            $response = \cairo_go_call($op, $payload);
            if (isset($response['error'])) {
                throw new \RuntimeException((string) $response['error']);
            }
            return $response['result'] ?? null;
        }
    }
}
