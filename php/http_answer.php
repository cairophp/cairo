<?php
/**
 * How an HTTP request is answered: the scheduler hands each "http" event to
 * HttpAnswer::answer(), inside the request's fiber.
 */

declare(strict_types=1);

namespace Cairo\Internal {

    use Cairo\App;
    use Cairo\Context;
    use Cairo\Http;

    /** Runs the handler of an HTTP request and sends its response; one per PHP thread. */
    final class HttpAnswer implements Answer
    {
        private readonly App $app;
        /** @var array<int, mixed> route id => its handler, a framework object already wrapped */
        private array $handlers = [];

        public function __construct(private readonly Scheduler $scheduler)
        {
            $this->app = $scheduler->app();
        }

        private function handler(int $id): mixed
        {
            if (array_key_exists($id, $this->handlers)) {
                return $this->handlers[$id];
            }
            $handler = $this->scheduler->resolve($id);
            if (is_object($handler) && !$handler instanceof \Closure) {
                $handler = Http\Adapter::wrap($handler);
            }
            return $this->handlers[$id] = $handler;
        }

        /** @param array<string, mixed> $event */
        public function answer(array $event): void
        {
            $isolated = $this->app->isolated();
            if ($isolated) {
                Superglobals::fill($event);
            }
            if (isset($event['files'])) {
                Form::track($event['files']);
            }
            $id = (int) $event['id'];
            try {
                $handler = $this->handler((int) $event['route']);
                $run = static fn () => $handler(new Http\Request($event), Context::current());
                $response = $isolated ? self::classic($run) : \cairo_guard($run);
                if (!$response instanceof Http\Response) {
                    $response = is_string($response) ? Http\Response::text($response) : Http\Response::json($response);
                }
                if (($writer = $response->writer()) !== null) {
                    \cairo_guard($writer);
                }
            } catch (Context\DeadlineExceeded) {
                $response = Http\Response::text("Gateway Timeout\n", 504);
            } catch (Context\Canceled) {
                // The client went away; nobody reads this, and it is not an error.
                $response = Http\Response::text("Client Closed Request\n", 499);
            } catch (\Throwable $e) {
                error_log('cairo: ' . $e);
                $response = Http\Response::text("Internal Server Error\n", 500);
            }
            if (isset($event['files'])) {
                Form::untrack($event['files']);   // Go removes what is left once we respond
            }
            $began = HttpOutput::$began[$id] ?? 0;
            if ($began === 0) {
                Context::current()->markAnswered();
                \cairo_respond_http($id, $response->status, $response->headers, $response->body);
                return;
            }
            unset(HttpOutput::$began[$id]);
            if ($began === HttpOutput::STREAMING) {
                // The head is with the client: all that is left to say is
                // how the handler ended, and Go ends the stream by it.
                Context::current()->markAnswered();
                \cairo_respond_http($id, $response->status, [], '');
            }
        }

        /**
         * Classic PHP style: echo and header() become the response, unless
         * the handler returns one. flush() sends what is written so far, and
         * the answer is a stream from then on.
         */
        private static function classic(\Closure $run): mixed
        {
            $out = HttpOutput::classic();
            try {
                $response = \cairo_guard($run);
            } finally {
                $output = $out->end();
            }
            if ($out->stream !== null) {
                $out->stream->write($output);
                return new Http\Response();
            }
            if ($response !== null) {
                return $response;
            }
            [$status, $headers] = HttpOutput::head();
            return new Http\Response($status, $headers, $output);
        }

        /** @param array<string, mixed> $event */
        public function route(array $event): string
        {
            return $this->app->routeName((int) $event['route']);
        }
    }

    Scheduler::answerWith('http', static fn (Scheduler $scheduler): Answer => new HttpAnswer($scheduler));
}
