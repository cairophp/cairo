<?php
/**
 * The gRPC server on the PHP side: Cairo\Grpc\Server declares it; with
 * the status codes, the exception that carries one and the server's end of a
 * stream.
 */

declare(strict_types=1);

namespace Cairo\Grpc {

    use Cairo\App;

    final class Server
    {
        /** @var list<string> */
        private array $protos = [];
        /** @var list<array{id:int,name:string}> */
        private array $services = [];
        private float $maxDuration = 0;
        /** @var list<\Cairo\Middleware> */
        private array $middleware = [];

        /** Add Go-side middleware: requestId(), accessLog(), rateLimit(); see Cairo\Middleware. */
        public function use(\Cairo\Middleware $middleware): static
        {
            $this->middleware[] = $middleware;
            return $this;
        }

        /** @internal */
        public function __construct(private readonly App $app, private readonly string $listen)
        {
        }

        /** A .proto file defining the services. Go compiles it at startup. */
        public function proto(string $file): static
        {
            $this->protos[] = $file;
            return $this;
        }

        /**
         * @param string $name fully qualified service name, e.g. 'helloworld.Greeter'
         * @param object|class-string $handler an object, or a class created once per thread.
         *        Each RPC method is called as $handler->Method(array $request, Context $ctx): array
         */
        public function service(string $name, object|string $handler): static
        {
            $id = $this->app->register($handler, $name);
            $this->services[] = ['id' => $id, 'name' => $name];
            return $this;
        }

        /** Upper bound in seconds for any call, applied when the client sets a later or no deadline. */
        public function maxDuration(float $seconds): static
        {
            $this->maxDuration = $seconds;
            return $this;
        }

        /** @internal */
        public function spec(): array
        {
            return [
                'listen' => $this->listen,
                'protos' => $this->protos,
                'max_duration' => $this->maxDuration,
                'services' => $this->services,
                'middleware' => array_map(static fn ($m) => $m->spec(), $this->middleware),
            ];
        }
    }

    /** gRPC status codes. */
    final class Code
    {
        public const OK = 0;
        public const CANCELLED = 1;
        public const UNKNOWN = 2;
        public const INVALID_ARGUMENT = 3;
        public const DEADLINE_EXCEEDED = 4;
        public const NOT_FOUND = 5;
        public const ALREADY_EXISTS = 6;
        public const PERMISSION_DENIED = 7;
        public const RESOURCE_EXHAUSTED = 8;
        public const FAILED_PRECONDITION = 9;
        public const ABORTED = 10;
        public const OUT_OF_RANGE = 11;
        public const UNIMPLEMENTED = 12;
        public const INTERNAL = 13;
        public const UNAVAILABLE = 14;
        public const DATA_LOSS = 15;
        public const UNAUTHENTICATED = 16;
    }

    /** Throw from a handler to return a gRPC status to the client. */
    class Exception extends \RuntimeException
    {
        public function __construct(int $code, string $message = '', ?\Throwable $previous = null)
        {
            parent::__construct($message, $code, $previous);
        }
    }

    /**
     * The stream of a streaming method being served. recv() returns the
     * client's next message (a generated object when the service implements
     * a generated interface, else a protojson array), or null once the client
     * has finished; send() sends one reply. Generated interfaces type it as
     * ServerStream<Request, Reply>.
     *
     * @template TRequest of array<string, mixed>|object
     * @template TReply of array<string, mixed>|object
     * @implements \IteratorAggregate<int, TRequest>
     */
    final class ServerStream implements \IteratorAggregate
    {
        /** @internal */
        public function __construct(private readonly int $id, private readonly ?string $class = null)
        {
        }

        /** @return TRequest|null */
        public function recv(): array|object|null
        {
            $r = Stream::call('grpc.server.recv', ['id' => $this->id]);
            if (!empty($r['eof'])) {
                return null;
            }
            return Stream::decode($r['message'], $this->class);
        }

        /** @param TReply $message */
        public function send(array|object $message): void
        {
            // Queued without a fiber switch while Go's reply queue has room;
            // a full queue waits in a Go operation instead.
            $payload = ['id' => $this->id, 'message' => Stream::encode($message)];
            $r = \Cairo\Internal\Go::callNow('grpc.server.trySend', $payload);
            if (!is_array($r) || !($r['ok'] ?? false)) {
                throw new Exception((int) ($r['code'] ?? Code::UNKNOWN), (string) ($r['error'] ?? 'stream failed'));
            }
            if (!($r['queued'] ?? false)) {
                Stream::call('grpc.server.send', $payload);
            }
        }

        /** @return \Generator<int, TRequest> */
        public function getIterator(): \Generator
        {
            while (($m = $this->recv()) !== null) {
                yield $m;
            }
        }
    }
}
