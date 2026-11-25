<?php
/**
 * A streamed HTTP answer: Cairo\Http\Stream and Events, which a handler gets
 * from Response::stream() and Response::events(), and how the output of code
 * that answers with echo reaches the client (Cairo\Internal\HttpOutput).
 */

declare(strict_types=1);

namespace Cairo\Http {

    use Cairo\Context;
    use Cairo\Internal\Go;
    use Cairo\Internal\HttpOutput;

    /**
     * The body of an answer that is sent while its handler runs
     * (Response::stream()). The status and the headers are with the client
     * before the first write().
     */
    final class Stream
    {
        /** @internal */
        public function __construct(private readonly int $id)
        {
        }

        /**
         * @internal Begins the answer of the running request.
         *
         * @param array<string, string|list<string>> $headers
         */
        public static function open(int $status, array $headers): self
        {
            $lists = [];
            foreach ($headers as $name => $value) {
                $lists[(string) $name] = array_map('strval', (array) $value);
            }
            $stream = new self((int) Go::call('http.stream.open', ['status' => $status, 'headers' => $lists]));
            HttpOutput::$began[Context::current()->requestId] = HttpOutput::STREAMING;
            return $stream;
        }

        /**
         * Sends $bytes. It returns at once while Go has room for them, and
         * waits, its fiber only, behind a client that reads slowly. Pieces
         * written together leave together.
         *
         * @throws Context\Canceled the client has gone
         */
        public function write(string $bytes): void
        {
            if ($bytes === '') {
                return;
            }
            $queued = \cairo_http_send($this->id, $bytes);
            if ($queued === false) {
                $queued = Go::call('http.stream.write', ['id' => $this->id, 'data' => $bytes]) ? true : -1;
            }
            if ($queued !== true) {
                throw $queued === -2 ? new \LogicException('the answer has ended') : new Context\Canceled();
            }
        }
    }

    /**
     * The events of a Server-Sent Events answer (Response::events()), which a
     * browser reads with EventSource.
     */
    final class Events
    {
        /** @internal */
        public function __construct(private readonly Stream $out)
        {
        }

        /**
         * Sends one event. $event names it for addEventListener(); without
         * a name it is a "message". The browser sends the last $id it saw
         * as the Last-Event-ID header when it connects again.
         *
         * @throws Context\Canceled the client has gone
         */
        public function send(string $data, ?string $event = null, ?string $id = null): void
        {
            $lines = $event === null ? '' : 'event: ' . self::line($event) . "\n";
            if ($id !== null) {
                $lines .= 'id: ' . self::line($id) . "\n";
            }
            // Every line of the data is a field of its own.
            $data = strtr($data, ["\r\n" => "\ndata: ", "\r" => "\ndata: ", "\n" => "\ndata: "]);
            $this->out->write($lines . 'data: ' . $data . "\n\n");
        }

        /** A line that the browser ignores: what keeps a proxy from closing a quiet stream. */
        public function comment(string $text = ''): void
        {
            $this->out->write(': ' . self::line($text) . "\n\n");
        }

        /** Tells the browser how long to wait before it connects again. */
        public function retry(int $milliseconds): void
        {
            $this->out->write('retry: ' . max(0, $milliseconds) . "\n\n");
        }

        /** A value with a line break would begin a field of the sender's choosing. */
        private static function line(string $value): string
        {
            if (strpbrk($value, "\r\n") !== false) {
                throw new \ValueError('an event name, id or comment is one line');
            }
            return $value;
        }
    }
}

namespace Cairo\Internal {

    use Cairo\Context;
    use Cairo\Http;

    /**
     * What a fiber writes with echo while it answers a request, on its way
     * to the client: for classic PHP in isolated mode, where echo and
     * header() are the answer, and for a framework's streamed response.
     * http_stream.c takes the fiber's output and calls drain() at flush(),
     * and when it has grown large.
     */
    final class HttpOutput
    {
        public const STREAMING = 1;   // the head is with the client
        public const ANSWERED = 2;    // the whole answer is (fastcgi_finish_request())

        /** @var array<int, int> request id => how far its answer is, for HttpAnswer */
        public static array $began = [];

        /** @var ?\WeakMap<object, self> by fiber, or by $stack */
        private static ?\WeakMap $of = null;
        /** What stands for the thread's own stack, where a classic request runs. */
        private static ?\stdClass $stack = null;

        public ?Http\Stream $stream = null;

        /** The output buffer that stands for the SAPI's in classic PHP; 0 without one. */
        private int $level = 0;

        /**
         * What classic PHP's answer holds before it streams, once its code has
         * closed the buffer that stands for the SAPI's (WordPress closes every
         * level at its end): a page goes whole, with its length.
         */
        private const CLASSIC_HOLD = 1 << 20;

        private static function key(): object
        {
            return \Fiber::getCurrent() ?? (self::$stack ??= new \stdClass());
        }

        private function __construct(int $hold = 8192)
        {
            self::$of ??= new \WeakMap();
            self::$of[self::key()] = $this;
            \cairo_http_capture(true, $hold);
        }

        /** Classic PHP: the running fiber's echo and header() are its answer. */
        public static function classic(): self
        {
            $out = new self(self::CLASSIC_HOLD);
            header_remove();
            http_response_code(200);
            ob_start();
            $out->level = ob_get_level();
            return $out;
        }

        /**
         * The running fiber's answer begins now, with this head, and what
         * $write echoes is its body: a framework's streamed response.
         *
         * @param array<string, string|list<string>> $headers
         * @param callable(): mixed $write
         */
        public static function send(int $status, array $headers, callable $write): void
        {
            $out = self::$of[self::key()] ?? null;
            if ($out !== null) {
                // Classic PHP has taken the fiber's output already, and ends it.
                $out->stream ??= Http\Stream::open($status, $headers);
                $write();
                return;
            }
            $out = new self();
            try {
                $out->stream = Http\Stream::open($status, $headers);
                $write();
            } finally {
                $rest = $out->end();
            }
            $out->stream->write($rest);
        }

        /** @internal for http_stream.c: flush(), or the output has grown large */
        public static function drain(): void
        {
            $out = self::$of[self::key()] ?? null;
            if ($out === null) {
                return;
            }
            if ($out->level !== 0 && ob_get_level() === $out->level) {
                ob_flush();   // down to http_stream.c, and PHP notes that headers were sent
            }
            $bytes = \cairo_http_captured();
            if ((self::$began[Context::current()->requestId] ?? 0) === self::ANSWERED) {
                return;
            }
            $out->stream ??= Http\Stream::open(...self::head());
            $out->stream->write($bytes);
        }

        /**
         * Stops taking the fiber's output and returns what was not sent.
         * Classic PHP ends as a script does: what it left in buffers of its
         * own is output too.
         */
        public function end(): string
        {
            $rest = '';
            if ($this->level !== 0) {
                while (ob_get_level() > $this->level) {
                    ob_end_flush();
                }
                if (ob_get_level() === $this->level) {
                    $rest = (string) ob_get_clean();
                }
            }
            unset(self::$of[self::key()]);
            // What went down earlier (ob_flush(), a buffer the code closed) comes first.
            return \cairo_http_capture(false) . $rest;
        }

        /**
         * fastcgi_finish_request(): the client gets the whole answer now, and
         * the handler runs on. False where echo is not the answer, and for
         * an answer that has begun to stream, which ends with its handler.
         */
        public static function finish(): bool
        {
            $out = self::$of[self::key()] ?? null;
            $id = Context::current()->requestId;
            if ($out === null || $out->level === 0 || isset(self::$began[$id])) {
                if ($out !== null) {
                    self::drain();
                }
                return false;
            }
            while (ob_get_level() >= $out->level) {
                ob_end_flush();
            }
            [$status, $headers] = self::head();
            self::$began[$id] = self::ANSWERED;
            Context::current()->markAnswered();
            \cairo_respond_http($id, $status, $headers, \cairo_http_captured());
            return true;
        }

        /**
         * headers_send(): Early Hints (103) from the headers set with
         * header(), or the head of the answer itself.
         */
        public static function headersSend(int $status): int
        {
            $out = self::$of[self::key()] ?? null;
            if ($status >= 100 && $status < 200) {
                Go::call('http.hints', ['status' => $status, 'headers' => self::headers()]);
                if ($out === null) {
                    // In a handler that returns its response, header() is
                    // the thread's and must not reach another request.
                    header_remove();
                }
                return $status;
            }
            if ($out !== null && $out->level !== 0 && $out->stream === null) {
                http_response_code($status);
                self::drain();
            }
            return $status;
        }

        /**
         * The head of a classic answer: the status and headers that the
         * code set, with PHP's default content type.
         *
         * @return array{0: int, 1: array<string, list<string>>}
         */
        public static function head(): array
        {
            $headers = self::headers();
            if (!isset(array_change_key_case($headers)['content-type'])) {
                $headers['Content-Type'] = ['text/html; charset=UTF-8'];
            }
            return [http_response_code() ?: 200, $headers];
        }

        /** @return array<string, list<string>> the headers set with header() */
        private static function headers(): array
        {
            $out = [];
            foreach (headers_list() as $line) {
                [$name, $value] = array_pad(explode(':', $line, 2), 2, '');
                $out[trim($name)][] = ltrim($value);
            }
            return $out;
        }
    }
}

namespace {

    // What classic PHP code calls, where a server runs it.
    if (PHP_SAPI === 'cairo') {
        /**
         * Sends the headers set with header() now: with 103 as Early Hints,
         * which a browser uses to preload while the page is made, or else
         * as the head of the answer.
         */
        function headers_send(int $status = 200): int
        {
            return \Cairo\Internal\HttpOutput::headersSend($status);
        }

        /**
         * Sends the answer written so far to the client as the whole
         * answer; the code after it still runs. Only where echo is the
         * answer (isolated mode), and false otherwise.
         */
        function fastcgi_finish_request(): bool
        {
            return \Cairo\Internal\HttpOutput::finish();
        }
    }
}
