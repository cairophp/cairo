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
     *          ->use(Middleware::cors(origins: ['https://app.example']))
     *          ->use(Middleware::rateLimit(perSecond: 50, burst: 100))
     *          ->use(Middleware::timeout(30))
     *          ->use(Middleware::gzip());
     *
     * gRPC servers take requestId(), accessLog(), rateLimit() and timeout().
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

        /**
         * Cross-origin requests. Preflights are answered in Go; PHP never sees them.
         * @param list<string> $origins allowed origins, or ['*']
         */
        public static function cors(
            array $origins,
            array $methods = [],
            array $headers = [],
            array $expose = [],
            bool $credentials = false,
            int $maxAge = 0,
        ): self {
            return new self('cors', [
                'origins' => array_values($origins), 'methods' => array_values($methods),
                'headers' => array_values($headers), 'expose' => array_values($expose),
                'credentials' => $credentials, 'max_age' => $maxAge,
            ]);
        }

        /**
         * A token bucket: $perSecond requests a second, bursts up to $burst.
         * Over the limit, HTTP answers 429 with Retry-After and gRPC answers
         * RESOURCE_EXHAUSTED. $by is 'ip' (a bucket per client) or 'global'.
         */
        public static function rateLimit(float $perSecond, int $burst = 0, string $by = 'ip'): self
        {
            return new self('rate_limit', ['per_second' => $perSecond, 'burst' => $burst, 'by' => $by]);
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

        /** Gzips responses of at least $minBytes for clients that accept it. HTTP only. */
        public static function gzip(int $minBytes = 1024): self
        {
            return new self('gzip', ['min_bytes' => $minBytes]);
        }

        /** @internal */
        public function spec(): array
        {
            return ['name' => $this->name, 'options' => (object) $this->options];
        }
    }
}
