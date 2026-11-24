<?php
/**
 * WebSockets and topics: Cairo\Http\WebSocket, which a handler gets from
 * Response::websocket(), its Message, and Cairo\Http\Topic, which publishes
 * to the WebSockets and the Server-Sent Events answers that listen.
 */

declare(strict_types=1);

namespace Cairo\Http {

    use Cairo\Context;
    use Cairo\Internal\Go;
    use Cairo\Internal\HttpOutput;

    /** One message of a WebSocket. */
    final class Message
    {
        public function __construct(
            public readonly string $data,
            public readonly bool $binary = false,
        ) {
        }
    }

    /**
     * A WebSocket, in the fiber of the request that asked for it
     * (Response::websocket()). The socket ends when its handler returns.
     *
     * A client that leaves does not cut its handler off: receive() still
     * returns what the client had sent, then null, and send() throws.
     */
    final class WebSocket
    {
        /** A status that a server may close with: the standard's, and 3000 to 4999 for the app's own. */
        private const STATUSES = [1000, 1001, 1003, 1007, 1008, 1009, 1011, 1012, 1013];

        private function __construct(
            private readonly int $id,
            /** The subprotocol that was agreed on, '' for none. */
            public readonly string $protocol,
        ) {
        }

        /**
         * @internal Makes the running request's connection a WebSocket and
         * runs $handler with it. A handshake that fails is answered by Go,
         * and $handler does not run.
         *
         * @param callable(self): void $handler
         * @param array{origins: list<string>, protocols: list<string>, max_message: int, ping: float} $options
         */
        public static function serve(callable $handler, array $options): void
        {
            $opened = Go::call('http.ws.open', $options);
            HttpOutput::$began[Context::current()->requestId] = HttpOutput::STREAMING;
            if ($opened !== null) {
                $handler(new self((int) $opened[0], (string) $opened[1]));
            }
        }

        /**
         * The next message of the client. It waits, its fiber only, until
         * one arrives, and returns null once the client has closed or gone
         * and all that it sent has been received.
         *
         * @throws Context\Canceled the request was ended, by a timeout or a server that stops
         */
        public function receive(): ?Message
        {
            while (true) {
                $taken = \cairo_ws_take($this->id);
                if (is_array($taken)) {
                    return new Message($taken[0], $taken[1]);
                }
                if ($taken) {
                    return null;
                }
                Go::call('http.ws.wait', ['id' => $this->id]);
            }
        }

        /**
         * Sends a message: text, which must be UTF-8, or binary. It returns
         * at once while Go has room for it, and waits, its fiber only,
         * behind a client that reads slowly.
         *
         * @throws Context\Canceled the client has gone
         */
        public function send(string $data, bool $binary = false): void
        {
            $queued = \cairo_ws_send($this->id, $data, $binary);
            if ($queued === false) {
                $queued = Go::call('http.ws.send', ['id' => $this->id, 'data' => $data, 'binary' => $binary]) ? true : -1;
            }
            if ($queued !== true) {
                throw $queued === -2 ? new \LogicException('the socket has ended') : new Context\Canceled();
            }
        }

        /**
         * Closes the socket with a status of your choosing, once what was
         * sent is with the client. A handler that just returns closes it
         * with 1000; statuses 3000 to 4999 are an app's own. receive()
         * returns what the client had still sent, then null.
         */
        public function close(int $code = 1000, string $reason = ''): void
        {
            if (!in_array($code, self::STATUSES, true) && ($code < 3000 || $code > 4999)) {
                throw new \ValueError("$code is not a status that a server closes a WebSocket with");
            }
            if (strlen($reason) > 123) {
                throw new \ValueError('the reason of a close has 123 bytes at most');
            }
            try {
                Go::call('http.ws.close', ['id' => $this->id, 'code' => $code, 'reason' => $reason]);
            } catch (Context\Canceled) {
                // The request was ended, and its socket with it.
            }
        }

        /**
         * The client gets what is published to these topics
         * (Topic::publish()) as text messages, sent by Go: this fiber does
         * not run for them.
         */
        public function subscribe(string ...$topics): void
        {
            Go::call('http.ws.subscribe', ['id' => $this->id, 'topics' => array_values($topics)]);
        }

        /** Ends that for these topics. A socket that ends leaves all of its topics. */
        public function unsubscribe(string ...$topics): void
        {
            Go::call('http.ws.unsubscribe', ['id' => $this->id, 'topics' => array_values($topics)]);
        }
    }

    /**
     * Topics: a message is published once and Go sends it to every client
     * that listens, the WebSockets that subscribed (WebSocket::subscribe())
     * and the Server-Sent Events answers of Response::eventsFrom(). The
     * topics are this server process's, and nothing is kept: a client
     * hears what is published while it listens.
     */
    final class Topic
    {
        /**
         * Publishes $data to $topic and returns how many clients listen to
         * it. It does not wait for them, and may be called from anywhere:
         * a handler, a task, defer(). A WebSocket gets $data as a text
         * message; a Server-Sent Events client gets an event with it,
         * named $event when given, and with $id.
         *
         * A client that reads more slowly than its topics are published to
         * is dropped once 64 messages wait for it, and connects again.
         */
        public static function publish(string $topic, string $data, ?string $event = null, ?string $id = null): int
        {
            $listening = \cairo_topic_publish($topic, $data, $event ?? '', $id ?? '');
            if ($listening >= 0) {
                return $listening;
            }
            return (int) Go::call('http.topics.publish', [
                'topic' => $topic, 'data' => $data, 'event' => $event ?? '', 'id' => $id ?? '',
            ]);
        }

        /**
         * @internal Makes the running request's answer the events of
         * $topics, which Go serves once this fiber has ended.
         *
         * @param list<string> $topics
         * @param array<string, string|list<string>> $headers
         */
        public static function listen(array $topics, array $headers, float $keepAlive): void
        {
            $lists = [];
            foreach ($headers as $name => $value) {
                $lists[(string) $name] = array_map('strval', (array) $value);
            }
            Go::call('http.topics.listen', ['topics' => $topics, 'headers' => $lists, 'keep_alive' => $keepAlive]);
            HttpOutput::$began[Context::current()->requestId] = HttpOutput::STREAMING;
        }
    }
}
