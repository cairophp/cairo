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

    /** @internal cairo serve's entry (core.Options.Entry). */
    final class Plain
    {
        private const SYMFONY_RUNTIME = 'Cairo\\Symfony\\Runtime';

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
            /** @var array{path: string, listen: string} $args */
            $args = $info['args'];
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

        /** @param array{path: string, listen: string} $args */
        private static function app(array $args): App
        {
            [$root, $front] = is_dir($args['path']) ? [$args['path'], 'index.php'] : [dirname($args['path']), basename($args['path'])];
            $app = (new App())->concurrency('classic')->servedByEntry();
            $app->http($args['listen'])->scripts($root, $front);
            return $app;
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

        /** @internal for Declares: Cairo's Symfony runtime is the one chosen for $file's app */
        public static function symfonyRuntime(string $file): bool
        {
            $chosen = $_SERVER['APP_RUNTIME'] ?? $_ENV['APP_RUNTIME'] ?? getenv('APP_RUNTIME');
            if (is_string($chosen) && $chosen !== '') {
                return ltrim($chosen, '\\') === self::SYMFONY_RUNTIME;
            }
            for ($dir = dirname($file); ; $dir = dirname($dir)) {
                if (is_file($dir . '/composer.json')) {
                    $composer = json_decode((string) file_get_contents($dir . '/composer.json'), true);
                    return ltrim((string) ($composer['extra']['runtime']['class'] ?? ''), '\\') === self::SYMFONY_RUNTIME;
                }
                if (dirname($dir) === $dir) {
                    return false;
                }
            }
        }
    }

    /**
     * Whether a PHP file declares a Cairo app, read with PHP's tokenizer and
     * never run: running it would run a plain script's code at start.
     *
     * @internal
     */
    final class Declares
    {
        public static function app(string $file): bool
        {
            $tokens = \PhpToken::tokenize((string) file_get_contents($file));
            $namespaced = false;
            $imported = false;   // `use Cairo;`: Cairo\App then means \Cairo\App in a namespace too
            $runtime = false;
            foreach ($tokens as $i => $token) {
                if ($token->is(T_NAMESPACE)) {
                    $namespaced = true;
                } elseif ($token->is(T_USE) && ($name = self::nameAfter($tokens, $i)) !== null) {
                    $name = ltrim($name, '\\');
                    if ($name === 'Cairo\\App' || ($name === 'Cairo\\' && self::groupNames($tokens, $i, 'App'))) {
                        return true;
                    }
                    $imported = $imported || $name === 'Cairo';
                } elseif ($token->is(T_NAME_FULLY_QUALIFIED) && $token->text === '\\Cairo\\App') {
                    return true;
                } elseif ($token->is(T_NAME_QUALIFIED) && $token->text === 'Cairo\\App' && (!$namespaced || $imported)) {
                    return true;
                } elseif ($token->is(T_CONSTANT_ENCAPSED_STRING) && str_ends_with(trim($token->text, '\'"'), 'autoload_runtime.php')) {
                    $runtime = true;
                }
            }
            return $runtime && Plain::symfonyRuntime($file);
        }

        /**
         * The name a `use` at $i imports, or null when none follows (a
         * closure's use, or a trait's inside a class, which import nothing
         * that matters here).
         *
         * @param list<\PhpToken> $tokens
         */
        private static function nameAfter(array $tokens, int $i): ?string
        {
            for ($j = $i + 1; isset($tokens[$j]); $j++) {
                $token = $tokens[$j];
                if ($token->isIgnorable()) {
                    continue;
                }
                if ($token->is([T_FUNCTION, T_CONST])) {
                    return null;
                }
                if ($token->is([T_NAME_QUALIFIED, T_NAME_FULLY_QUALIFIED, T_STRING])) {
                    $next = $tokens[$j + 1] ?? null;
                    return $next !== null && $next->is(T_NS_SEPARATOR) ? $token->text . '\\' : $token->text;
                }
                return null;
            }
            return null;
        }

        /**
         * Whether the group `use Cairo\{...}` at $i names $name.
         *
         * @param list<\PhpToken> $tokens
         */
        private static function groupNames(array $tokens, int $i, string $name): bool
        {
            for ($j = $i + 1; isset($tokens[$j]) && $tokens[$j]->text !== ';'; $j++) {
                if ($tokens[$j]->is(T_STRING) && $tokens[$j]->text === $name) {
                    return true;
                }
            }
            return false;
        }
    }
}
