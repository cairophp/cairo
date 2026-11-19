<?php
/**
 * Cairo\Internal\Go: how PHP asks Go to run one of its registered
 * operations (see internal/sapi/goops.go).
 */

declare(strict_types=1);

namespace Cairo\Internal {

    use Cairo\Context;

    /**
     * Runs a Go operation (see internal/sapi/goops.go): in a Cairo fiber the
     * fiber suspends while the fiber's goroutine runs it; elsewhere the
     * thread waits for Go.
     */
    final class Go
    {
        /** @param array<string, mixed> $payload */
        public static function call(string $op, array $payload): mixed
        {
            if (Scheduler::running() && Context::current()->fiberId !== 0) {
                $json = json_encode($payload, JSON_THROW_ON_ERROR | JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE);
                return Scheduler::instance()->call($op, $json);
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
            $json = json_encode($payload, JSON_THROW_ON_ERROR | JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE);
            $response = json_decode(\cairo_go_call($op, $json), true, 512, JSON_THROW_ON_ERROR);
            if (isset($response['error'])) {
                throw new \RuntimeException((string) $response['error']);
            }
            return $response['result'] ?? null;
        }
    }
}
