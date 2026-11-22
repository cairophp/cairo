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
            try {
                if ($isolated) {
                    // Classic PHP style: echo and header() become the response.
                    ob_start();
                    header_remove();
                    http_response_code(200);
                }
                try {
                    $handler = $this->handler((int) $event['route']);
                    $response = \cairo_guard(static fn () => $handler(new Http\Request($event), Context::current()));
                } finally {
                    $output = $isolated ? (string) ob_get_clean() : '';
                }
                if ($isolated && $response === null) {
                    $headers = self::sentHeaders();
                    if (!isset(array_change_key_case($headers)['content-type'])) {
                        $headers['Content-Type'] = ['text/html; charset=UTF-8'];   // PHP's default
                    }
                    $response = new Http\Response((int) (http_response_code() ?: 200), $headers, $output);
                }
                if (!$response instanceof Http\Response) {
                    $response = is_string($response) ? Http\Response::text($response) : Http\Response::json($response);
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
            Context::current()->markAnswered();
            if (isset($event['files'])) {
                Form::untrack($event['files']);   // Go removes what is left once we respond
            }
            \cairo_respond_http((int) $event['id'], $response->status, $response->headers, $response->body);
        }

        /** @param array<string, mixed> $event */
        public function route(array $event): string
        {
            return $this->app->routeName((int) $event['route']);
        }

        /** @return array<string, list<string>> the headers set with header() */
        private static function sentHeaders(): array
        {
            $out = [];
            foreach (headers_list() as $line) {
                [$name, $value] = array_pad(explode(':', $line, 2), 2, '');
                $out[trim($name)][] = ltrim($value);
            }
            header_remove();
            return $out;
        }
    }

    Scheduler::answerWith('http', static fn (Scheduler $scheduler): Answer => new HttpAnswer($scheduler));
}
