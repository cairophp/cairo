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
    use Cairo\Middleware;

    /** @internal cairo serve's entry (core.Options.Entry). */
    final class Plain
    {
        /**
         * The App of the path cairo serve was given: what a declaration
         * returns, or one that serves the scripts. On a request thread of the
         * latter, returns null: the thread's requests are served in C from
         * its next PHP request on. An App back is for the scheduler, for an
         * event that is not such a request.
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
            if ($info['mode'] === 'serve') {
                // No declaration runs here, so the thread has none to check,
                // and PHP leaves the environment out of $_SERVER (cairo.c).
                \cairo_thread_check_passed();
                \cairo_plain_runtime(__DIR__, (bool) $info['reloaded']);
                if (\cairo_cserve()) {
                    // From the thread's next PHP request on, its requests
                    // are PHP's own SAPI requests (plain_request.c).
                    \cairo_thread_again();
                    return null;
                }
            }
            load($info);
            return self::app($args);
        }

        /**
         * The App that serves the scripts.
         *
         * @param array{path: string, listen: string, timeout: float} $args
         */
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
    }
}
