<?php
/**
 * `cairo serve` on a folder or on a PHP file that is not a declaration: Cairo
 * serves the scripts as a web server in front of PHP-FPM does, in classic
 * mode, with no server.php. Every request runs only its script, in a PHP
 * request of its own, and nothing of the app runs before it: no request can
 * see what another one, or a file at start, set.
 */

declare(strict_types=1);

namespace Cairo\Internal {

    use Cairo\App;
    use Cairo\Context;
    use Cairo\Middleware;

    /** @internal cairo serve's entry (core.Options.Entry). */
    final class Plain
    {
        /** @var array<string, mixed> what cairo_info() said of this thread */
        private static array $info = [];

        /**
         * The App of the path cairo serve was given: what a declaration
         * returns, or one that serves the scripts. On a request thread of the
         * latter, serves the thread's request and returns null; an App back
         * is for the scheduler, for an event that is not such a request.
         *
         * @param array<string, mixed> $info
         */
        public static function entry(array $info): ?App
        {
            /** @var array{path: string, listen: string, timeout: float} $args */
            $args = $info['args'];
            if ($info['mode'] !== 'serve') {
                // The threads that send the declaration build it alike. A
                // request thread sends none, and reads no file for it.
                $args['timeout'] = $args['timeout'] ?: self::iniTimeout();
            }
            if ($info['mode'] === 'declare') {
                if (is_file($args['path']) && Declares::app($args['path'])) {
                    return (static fn (string $file): mixed => require $file)($args['path']);
                }
                return self::app($args);
            }
            if ($info['mode'] === 'serve' && self::serve($info)) {
                return null;
            }
            load($info);
            return self::app($args);
        }

        /** @param array{path: string, listen: string, timeout: float} $args */
        private static function app(array $args): App
        {
            [$root, $front] = is_dir($args['path']) ? [$args['path'], 'index.php'] : [dirname($args['path']), basename($args['path'])];
            $app = (new App())->concurrency('classic')->servedByEntry();
            $http = $app->http($args['listen'])->scripts($root, $front);
            if ($args['timeout'] > 0) {
                $http->use(Middleware::timeout($args['timeout']));
            }
            return $app;
        }

        /**
         * php.ini's max_execution_time, the deadline a script has under
         * PHP-FPM: Cairo sets it to 0 for its threads, which are PHP requests
         * that last, so it is read from the files PHP loaded, in PHP's order.
         * Wall time here, where PHP-FPM counts CPU time on Linux.
         */
        private static function iniTimeout(): float
        {
            $files = php_ini_loaded_file() === false ? [] : [php_ini_loaded_file()];
            $scanned = php_ini_scanned_files();
            if ($scanned !== false && $scanned !== '') {
                array_push($files, ...array_map('trim', explode(',', $scanned)));
            }
            $seconds = 0;
            foreach ($files as $file) {
                $ini = parse_ini_file($file);
                if ($ini !== false && isset($ini['max_execution_time'])) {
                    $seconds = (int) $ini['max_execution_time'];
                }
            }
            return (float) max(0, $seconds);
        }

        /**
         * Answers the thread's request as a classic request does, with only
         * the files that takes: the context, the superglobals, the output,
         * and the functions of Cairo's concurrency API, whose classes and
         * scheduler load on first use. False when the thread's event is not
         * a request, which the scheduler then takes.
         *
         * @param array<string, mixed> $info
         */
        private static function serve(array $info): bool
        {
            // No declaration runs here, so the thread has none to check, and
            // PHP leaves the environment out of $_SERVER (cairo.c), which
            // Superglobals::fill() would drop anyway: copying it was a fifth
            // of a hello request on a machine with 16 KB of environment.
            \cairo_thread_check_passed();
            foreach (['context', 'go', 'async', 'channel', 'offload', 'form', 'superglobals', 'http_stream'] as $file) {
                require_once __DIR__ . '/' . $file . '.php';
            }
            Runtime::mark();
            self::$info = $info;
            spl_autoload_register(self::loadAll(...));
            do {
                $event = \cairo_next_event(true);
            } while ($event === null);
            if ($event['type'] !== 'http' || !isset($event['script'])) {
                Runtime::$first = $event;
                return false;
            }
            $id = (int) $event['id'];
            $context = new Context($id, isset($event['deadline']) ? (float) $event['deadline'] : null);
            if (isset($event['values'])) {
                $context->setGoValues((array) $event['values']);
            }
            // As Scheduler::start() has a classic request: the calls that
            // come meanwhile wait for the thread's next PHP request, and this
            // one runs on the thread's own stack.
            \cairo_hold_calls();
            $deadline = $context->deadlineTick();
            \cairo_fiber_bind(null, $context, $id, $deadline === PHP_INT_MAX ? 0 : $deadline);
            try {
                self::answer($event, $id);
            } finally {
                $context->finish();
                $context->ended('canceled');
                \cairo_local_end($id);
                \cairo_fiber_bind(null, null, 0);
            }
            \cairo_thread_again();
            return true;
        }

        /**
         * Runs the script of $event and sends what it wrote, as HttpAnswer
         * answers a classic request.
         *
         * @param array<string, mixed> $event
         */
        private static function answer(array $event, int $id): void
        {
            Superglobals::fill($event);
            if (isset($event['files'])) {
                Form::track($event['files']);
            }
            $out = HttpOutput::classic();
            $failed = null;
            try {
                \cairo_guard(static fn () => Scripts::runScript($event['script'], (string) ($event['body'] ?? '')));
            } catch (Context\DeadlineExceeded) {
                $failed = [504, "Gateway Timeout\n"];
            } catch (Context\Canceled) {
                $failed = [499, "Client Closed Request\n"];
            } catch (\Throwable $e) {
                error_log('cairo: ' . $e);
                $failed = [500, "Internal Server Error\n"];
            } finally {
                $output = $out->end();
            }
            if ($failed === null && $out->stream !== null) {
                $out->stream->write($output);
            }
            [$status, $headers] = $failed === null ? HttpOutput::head() : [$failed[0], ['Content-Type' => ['text/plain; charset=utf-8']]];
            $body = $failed[1] ?? $output;
            $began = HttpOutput::$began[$id] ?? 0;
            if ($began === HttpOutput::ANSWERED) {
                return;   // fastcgi_finish_request() answered already
            }
            Context::current()->markAnswered();
            if ($began === HttpOutput::STREAMING) {
                // The head is with the client: how the script ended is all
                // that is left to say, and Go ends the stream by it.
                \cairo_respond_http($id, $failed[0] ?? 200, [], '');
                return;
            }
            \cairo_respond_http($id, $status, $headers, $body);
        }

        /**
         * A Cairo class that the files loaded so far do not have: the script
         * uses more of Cairo, so the whole runtime loads, with a scheduler
         * for what waits (await(), select(), a pool) to run its loop.
         */
        private static function loadAll(string $class): void
        {
            if (!str_starts_with($class, 'Cairo\\') || self::$info === []) {
                return;
            }
            $info = self::$info;
            self::$info = [];
            load($info);
            new Scheduler(self::app($info['args']));
        }
    }
}
