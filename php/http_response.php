<?php
/**
 * Cairo\Http\Response: what a handler returns, whole or as a body that
 * is sent while its writer runs.
 */

declare(strict_types=1);

namespace Cairo\Http {

    final class Response
    {
        /** What sends the body of a streamed answer, once the head is with the client. */
        private ?\Closure $writer = null;

        /** @param array<string, string|list<string>> $headers */
        public function __construct(
            public readonly int $status = 200,
            public readonly array $headers = [],
            public readonly string $body = '',
        ) {
        }

        /**
         * An answer whose body is sent while $writer runs: the client has
         * the status and the headers first, then every Stream::write() as it
         * is made, and the answer ends when $writer returns.
         *
         *     Response::stream(function (Stream $out) {
         *         foreach (rows() as $row) {
         *             $out->write(json_encode($row) . "\n");
         *         }
         *     }, headers: ['Content-Type' => 'application/x-ndjson']);
         *
         * $writer runs in the request's fiber and may wait like any handler.
         * When the client leaves, its next wait or write() throws
         * Cairo\Context\Canceled. A route with cache() cannot stream.
         *
         * @param callable(Stream): void $writer
         * @param array<string, string|list<string>> $headers
         */
        public static function stream(callable $writer, int $status = 200, array $headers = []): self
        {
            $response = new self($status, $headers);
            $response->writer = $writer(...);
            return $response;
        }

        /**
         * An answer that is the file at $path, which Go sends in place of
         * a body: with ranges and If-Modified-Since, and without holding
         * a PHP thread. The file must be under a directory given to
         * $http->sendfile(). The content type is the file name's, unless
         * $headers has one.
         *
         * @param array<string, string|list<string>> $headers
         */
        public static function file(string $path, array $headers = []): self
        {
            return new self(200, ['X-Sendfile' => $path] + $headers);
        }

        /**
         * A Server-Sent Events answer: $source sends events for as long as
         * it runs, and a browser reads them with EventSource.
         *
         *     Response::events(function (Events $events) use ($ctx) {
         *         while (true) {
         *             $events->send(json_encode(next_price()), event: 'price');
         *             \Cairo\sleep(1);
         *         }
         *     });
         *
         * Give the route noTimeout() when the server has a timeout.
         *
         * @param callable(Events): void $source
         * @param array<string, string|list<string>> $headers
         */
        public static function events(callable $source, array $headers = []): self
        {
            return self::stream(static fn (Stream $out) => $source(new Events($out)), 200, $headers + [
                'Content-Type' => 'text/event-stream',
                'Cache-Control' => 'no-cache',
                'X-Accel-Buffering' => 'no',   // a proxy of nginx's kind passes each event on
            ]);
        }

        /** @internal the writer of a streamed answer */
        public function writer(): ?\Closure
        {
            return $this->writer;
        }

        public static function text(string $body, int $status = 200): self
        {
            return new self($status, ['Content-Type' => 'text/plain; charset=utf-8'], $body);
        }

        public static function html(string $body, int $status = 200): self
        {
            return new self($status, ['Content-Type' => 'text/html; charset=utf-8'], $body);
        }

        public static function json(mixed $data, int $status = 200): self
        {
            $body = json_encode($data, JSON_THROW_ON_ERROR | JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE);
            return new self($status, ['Content-Type' => 'application/json'], $body);
        }
    }
}
