<?php
/**
 * Cairo\Middleware: the middleware a server declares and Go runs before
 * a request reaches PHP.
 */

declare(strict_types=1);

namespace Cairo {

    /**
     * Middleware that Go runs before a request reaches PHP. Declare it on a
     * server, in order (the first one runs first):
     *
     *     $http->use(Middleware::requestId())
     *          ->use(Middleware::accessLog())
     *          ->use(Middleware::maxBody(8 << 20))
     *          ->use(Middleware::timeout(30));
     *
     * gRPC servers take requestId(), accessLog() and timeout().
     */
    final class Middleware
    {
        private function __construct(public readonly string $name, public readonly array $options = [])
        {
        }

        /** Keeps a valid incoming X-Request-Id (gRPC: x-request-id) or sets a new one; it is echoed on the response. */
        public static function requestId(): self
        {
            return new self('request_id');
        }

        /** One log line per request: method, path, status, bytes, time, client, request id. */
        public static function accessLog(): self
        {
            return new self('access_log');
        }

        /** Rejects request bodies larger than $bytes with 413. HTTP only. */
        public static function maxBody(int $bytes): self
        {
            return new self('max_body', ['bytes' => $bytes]);
        }

        /**
         * Gives each request a deadline $seconds away (gRPC: the earlier of
         * this and the client's). Past it, PHP code is stopped with
         * Context\DeadlineExceeded even if it never waits on I/O, and the
         * client gets 504 or DEADLINE_EXCEEDED.
         */
        public static function timeout(float $seconds): self
        {
            return new self('timeout', ['seconds' => $seconds]);
        }

        /** @internal */
        public function spec(): array
        {
            return ['name' => $this->name, 'options' => (object) $this->options];
        }
    }
}
