<?php
/**
 * Cairo\Grpc\Client: a gRPC client whose calls and streams wait in Go.
 */

declare(strict_types=1);

namespace Cairo\Grpc {

    /**
     * A gRPC client over grpc-go, for unary calls. Messages are protojson
     * arrays (or generated message objects); Go encodes them with the
     * descriptors of the given .proto files. `cairo gen` also writes a typed
     * client per service (GreeterClient) that carries its descriptors.
     *
     *     $client = new Client('localhost:9000', ['protos' => [__DIR__ . '/greeter.proto']]);
     *     $reply = $client->call('/helloworld.Greeter/SayHello', ['name' => 'Cairo']);
     *
     * In a request handler or async() task only the fiber waits, and the
     * request's deadline becomes the call's deadline. Connections are shared
     * and reused across requests and threads.
     *
     * Options: protos (paths), descriptors (base64 FileDescriptorSet), tls
     * (true, or [cafile, cert, key, server_name, verify]), metadata
     * (sent with every call), timeout (seconds, default 30).
     */
    final class Client
    {
        /** @param array<string, mixed> $options */
        public function __construct(private readonly string $target, private readonly array $options = [])
        {
        }

        /**
         * Calls a unary method and returns the reply as a protojson array.
         * A status other than OK throws Cairo\Grpc\Exception with its code.
         *
         * @param array<string, mixed>|object $request an array, or a message with toArray()
         * @param array{metadata?: array<string, string|list<string>>, timeout?: float} $options
         * @return array<string, mixed>
         */
        public function call(string $method, array|object $request, array $options = []): array
        {
            return $this->invoke($method, $request, $options)->message;
        }

        /**
         * Like call(), with the reply's headers and trailers.
         *
         * @param array<string, mixed>|object $request
         * @param array{metadata?: array<string, string|list<string>>, timeout?: float} $options
         */
        public function invoke(string $method, array|object $request, array $options = []): Reply
        {
            $result = \Cairo\Internal\Go::call('grpc.call', $this->payload($method, $request, $options, 30));
            if (!($result['ok'] ?? false)) {
                throw new Exception((int) ($result['code'] ?? Code::UNKNOWN), (string) ($result['error'] ?? 'call failed'));
            }
            return new Reply($result['message'], $result['headers'] ?: [], $result['trailers'] ?: []);
        }

        /**
         * @param array<string, mixed>|object|null $request
         * @param array<string, mixed> $options
         * @return array<string, mixed>
         */
        private function payload(string $method, array|object|null $request, array $options, float $timeout): array
        {
            $tls = $this->options['tls'] ?? false;
            $tlsOpts = is_array($tls) ? $tls : [];
            $metadata = [];
            foreach ([$this->options['metadata'] ?? [], $options['metadata'] ?? []] as $set) {
                foreach ($set as $key => $value) {
                    $metadata[strtolower((string) $key)] = array_map('strval', (array) $value);
                }
            }
            return [
                'target' => $this->target,
                'method' => $method,
                'protos' => array_values((array) ($this->options['protos'] ?? [])),
                'descriptors' => (string) ($this->options['descriptors'] ?? ''),
                'message' => $request === null ? null : Stream::encode($request),
                'metadata' => (object) $metadata,
                'timeout' => (float) ($options['timeout'] ?? $this->options['timeout'] ?? $timeout),
                'tls' => [
                    'enabled' => $tls !== false,
                    'cafile' => (string) ($tlsOpts['cafile'] ?? ''),
                    'cert' => (string) ($tlsOpts['cert'] ?? ''),
                    'key' => (string) ($tlsOpts['key'] ?? ''),
                    'server_name' => (string) ($tlsOpts['server_name'] ?? ''),
                    'verify' => (bool) ($tlsOpts['verify'] ?? true),
                ],
            ];
        }

        /**
         * Opens a streaming call (client-streaming or bidirectional). $class
         * is the generated reply class recv() builds, or null for arrays.
         *
         * @param array{metadata?: array<string, string|list<string>>, timeout?: float} $options
         */
        public function stream(string $method, array $options = [], ?string $class = null): ClientStream
        {
            $r = Stream::call('grpc.stream.open', $this->payload($method, null, $options, 0));
            return new ClientStream((int) $r['id'], $class);
        }

        /**
         * Starts a server-streaming call with its one request; read the
         * replies with recv() or foreach.
         *
         * @param array<string, mixed>|object $request
         * @param array{metadata?: array<string, string|list<string>>, timeout?: float} $options
         */
        public function serverStream(string $method, array|object $request, array $options = [], ?string $class = null): ClientStream
        {
            $stream = $this->stream($method, $options, $class);
            $stream->send($request);
            $stream->closeSend();
            return $stream;
        }
    }

    /**
     * A streaming call made with Client::stream() or serverStream(), or a
     * generated client's streaming method. send() and closeSend() for the
     * requests, recv() for the replies (null at the end; a failed call
     * throws Cairo\Grpc\Exception). Iterating reads the replies. Generated
     * clients type it as ClientStream<Request, Reply>.
     *
     * @template TRequest of array<string, mixed>|object
     * @template TReply of array<string, mixed>|object
     * @implements \IteratorAggregate<int, TReply>
     */
    final class ClientStream implements \IteratorAggregate
    {
        private bool $closed = false;
        /** @var array<string, list<string>> */
        private array $headers = [];
        /** @var array<string, list<string>> */
        private array $trailers = [];

        /** @internal */
        public function __construct(private readonly int $id, private readonly ?string $class = null)
        {
        }

        public function __destruct()
        {
            $this->close();
        }

        /** @param TRequest $message */
        public function send(array|object $message): void
        {
            Stream::call('grpc.stream.send', ['id' => $this->id, 'message' => Stream::encode($message)]);
        }

        /** No more requests: the server sees the end of the client's stream. */
        public function closeSend(): void
        {
            Stream::call('grpc.stream.closeSend', ['id' => $this->id]);
        }

        /** @return TReply|null */
        public function recv(): array|object|null
        {
            $r = Stream::call('grpc.stream.recv', ['id' => $this->id]);
            if (!empty($r['eof'])) {
                $this->headers = $r['headers'] ?: [];
                $this->trailers = $r['trailers'] ?: [];
                return null;
            }
            return Stream::decode($r['message'], $this->class);
        }

        /**
         * For client-streaming methods: end the requests and read the one reply.
         *
         * @return TReply
         */
        public function closeAndRecv(): array|object
        {
            $this->closeSend();
            $reply = $this->recv() ?? throw new Exception(Code::INTERNAL, 'the server sent no reply');
            $this->recv();   // the end of the stream, with the trailers
            return $reply;
        }

        /** Cancels the call if it is still running. */
        public function close(): void
        {
            if (!$this->closed) {
                $this->closed = true;
                \cairo_go_call('grpc.stream.close', ['id' => $this->id]);
            }
        }

        /** @return array<string, list<string>> response headers, once the stream has ended */
        public function headers(): array
        {
            return $this->headers;
        }

        /** @return array<string, list<string>> trailers, once the stream has ended */
        public function trailers(): array
        {
            return $this->trailers;
        }

        /** @return \Generator<int, TReply> */
        public function getIterator(): \Generator
        {
            while (($m = $this->recv()) !== null) {
                yield $m;
            }
        }
    }

    /** @internal */
    final class Stream
    {
        /** @param array<string, mixed> $payload */
        public static function call(string $op, array $payload): array
        {
            $r = \Cairo\Internal\Go::call($op, $payload);
            if (!is_array($r) || !($r['ok'] ?? false)) {
                throw new Exception((int) ($r['code'] ?? Code::UNKNOWN), (string) ($r['error'] ?? 'stream failed'));
            }
            return $r;
        }

        public static function encode(array|object $message): mixed
        {
            if (is_object($message)) {
                $message = method_exists($message, 'toArray') ? $message->toArray() : get_object_vars($message);
            }
            return $message === [] ? new \stdClass() : $message;
        }

        public static function decode(mixed $message, ?string $class): array|object
        {
            $message = is_array($message) ? $message : [];
            return $class !== null ? $class::fromArray($message) : $message;
        }
    }

    final class Reply
    {
        /**
         * @param array<string, mixed> $message protojson
         * @param array<string, list<string>> $headers
         * @param array<string, list<string>> $trailers
         */
        public function __construct(
            public readonly array $message,
            public readonly array $headers,
            public readonly array $trailers,
        ) {
        }
    }
}
