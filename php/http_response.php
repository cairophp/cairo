<?php
/**
 * Cairo\Http\Response: what a handler returns, whole or as a body that
 * is sent while its writer runs.
 */

declare(strict_types=1);

namespace Cairo\Http {

    final class Response
    {
        /**
         * What answers in place of a body, while the request's call stays
         * open: it begins the answer itself (a stream, a WebSocket).
         */
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
            $response->writer = static fn () => $writer(Stream::open($status, $headers));
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

        /**
         * A Server-Sent Events answer that Go serves: the client gets what
         * is published to $topics (Topic::publish()) for as long as it
         * stays, and no PHP runs for it. The handler decides who may listen
         * to what, returns this, and its fiber ends.
         *
         *     $http->get('/news', fn (Request $req): Response =>
         *         Response::eventsFrom(['news', 'user.' . userOf($req)]));
         *
         * Go sends a comment every $keepAlive seconds, so that no proxy
         * closes a quiet connection (0 for none). Nothing is kept for a
         * client that connects again: it hears what is published from then.
         *
         * @param string|list<string> $topics 64 at most
         * @param array<string, string|list<string>> $headers
         */
        public static function eventsFrom(string|array $topics, array $headers = [], float $keepAlive = 15.0): self
        {
            $headers += [
                'Content-Type' => 'text/event-stream',
                'Cache-Control' => 'no-cache',
                'X-Accel-Buffering' => 'no',
            ];
            $response = new self(200, $headers);
            $response->writer = static fn () => Topic::listen(array_values((array) $topics), $headers, $keepAlive);
            return $response;
        }

        /**
         * A WebSocket: the connection becomes one, and $handler runs with
         * it in the request's fiber until it returns, which closes the
         * socket.
         *
         *     $http->get('/chat', fn (Request $req): Response => Response::websocket(function (WebSocket $ws): void {
         *         $ws->subscribe('room');
         *         while (($message = $ws->receive()) !== null) {
         *             Topic::publish('room', $message->data);
         *         }
         *     }));
         *
         * The handler of the route runs first, as for any request: it may
         * answer with another Response instead (a 401, say), and only this
         * one makes the handshake. A page of another site may not connect:
         * the browser's Origin must be the request's own host, or match one
         * of $origins (a host, with * for any run of characters:
         * 'app.example.com', '*.example.com'). $protocols are the
         * subprotocols the client may choose from, in your order of
         * preference. A message of the client over $maxMessage bytes closes
         * the socket. Go pings the client every $ping seconds and drops one
         * that does not answer (0 for no pings).
         *
         * Give the route noTimeout() when the server has a timeout.
         *
         * @param callable(WebSocket): void $handler
         * @param list<string> $origins
         * @param list<string> $protocols
         */
        public static function websocket(
            callable $handler,
            array $origins = [],
            array $protocols = [],
            int $maxMessage = 65536,
            float $ping = 30.0,
        ): self {
            $options = ['origins' => array_values($origins), 'protocols' => array_values($protocols),
                'max_message' => $maxMessage, 'ping' => $ping];
            $response = new self();
            $response->writer = static fn () => WebSocket::serve($handler, $options);
            return $response;
        }

        /** @internal what answers in place of a body */
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
