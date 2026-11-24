<?php
/**
 * Cairo runtime: where the PHP half of Cairo starts.
 *
 * Every PHP thread runs this file. It loads the core's files next to it (the
 * declaration, contexts, channels, tasks and the fiber scheduler), then the
 * files the extensions registered, and then the user's server.php, which
 * returns a Cairo\App describing the servers. On the boot thread that
 * description is sent to Go (the declare phase). On worker threads the file
 * then runs the fiber scheduler: each request runs in its own Fiber, and a
 * fiber that waits on Go-backed I/O is suspended so the thread can run others.
 * Under `cairo php` there is no server.php: the script of the command line
 * runs under the scheduler as the thread's one request, in a fiber.
 */

declare(strict_types=1);

namespace Cairo\Internal {

    use Cairo\App;

    /** Cairo's own PHP files: this one and those it loads at start. */
    final class Runtime
    {
        /** @var array<string, true> */
        public static array $files = [];
        /** declare, serve or offload until serve() has run; '' after */
        public static string $mode = '';
    }

    /**
     * Called inside the running fiber when Go interrupts the thread because a
     * request's deadline passed (see cairo_on_interrupt in cairo.c): whether
     * the fiber's code, in $file, is to be stopped.
     */
    function interrupt(?string $file): bool
    {
        if ($file !== null && isset(Runtime::$files[$file])) {
            return false;   // never inside Cairo's runtime (it answers the request); Go pokes again
        }
        $context = \Cairo\Context::current();
        return !$context->answered() && $context->err() instanceof \Cairo\Context\DeadlineExceeded;
    }

    function main(): void
    {
        // Go names the files of the extensions (RegisterPHP in internal/core):
        // those to load now, and the classes whose first use loads the others.
        $info = json_decode(\cairo_info(), true, 512, JSON_THROW_ON_ERROR);
        foreach ($info['php'] as $file) {
            require __DIR__ . '/' . $file;
        }
        // What this directory has loaded by now is the runtime; server.php and
        // what loads after it, Cairo's files included, counts as the app.
        Runtime::$files = array_fill_keys(array_filter(get_included_files(), static fn (string $file): bool => dirname($file) === __DIR__), true);
        $onUse = $info['classes'] + ['Cairo\\RequestLocal' => 'request_local.php'];
        spl_autoload_register(static function (string $class) use ($onUse): void {
            if (isset($onUse[$class])) {
                require_once __DIR__ . '/' . $onUse[$class];
            }
        });
        if ($info['mode'] === 'cli') {
            // Cairo's files are removed once the script begins (goCLILoaded),
            // so what a first use would load is loaded now.
            foreach (array_unique($onUse) as $file) {
                require_once __DIR__ . '/' . $file;
            }
            if ($info['script'] === 0) {
                \cairo_cli_script();   // not scheduled: the thread's own code, as under PHP
                return;
            }
            // The script is this thread's one request, which is what
            // 'isolated' says of a thread: it may wait with its only fiber.
            $scheduler = new Scheduler((new App())->concurrency('isolated'));
            if (!$scheduler->script($info['script'], static fn () => \cairo_cli_script())) {
                file_put_contents('php://stderr', "cairo: the script waits and nothing can wake it: every fiber is asleep, or code with an event loop of its own suspended one. CAIRO_SCHEDULER=off runs a script outside Cairo's scheduler\n");
                exit(255);
            }
            return;
        }
        if ($info['reloaded'] && PHP_VERSION_ID >= 80400) {
            // The code has changed since the server started (Engine.Reload):
            // opcache compares every file it has with the one on disk,
            // whatever php.ini says, or this thread would run the old code.
            ini_set('opcache.validate_timestamps', '1');
            ini_set('opcache.revalidate_freq', '0');
        } elseif ($info['reloaded']) {
            // New code beside the old is what fills opcache, and before 8.4
            // nothing tells that it is about to wipe its cache under the
            // threads that run (startgate.h): this thread compiles for itself.
            ini_set('opcache.enable', '0');
        }
        Runtime::$mode = $info['mode'];
        // The served file is the script, as under any server: a framework's
        // front controller finds itself there (Symfony's autoload_runtime.php).
        $_SERVER['SCRIPT_FILENAME'] = $info['server'];
        $app = (static fn (string $file): mixed => require $file)($info['server']);
        if (Runtime::$mode === '') {
            return;   // the file served its App itself
        }
        if (!$app instanceof App) {
            throw new \RuntimeException($info['server'] . ' must return a Cairo\App');
        }
        serve($app);
    }

    /**
     * Declare $app's servers and serve them on this thread, until the server
     * stops. server.php returns its App and Cairo calls this; a framework's
     * runner that never returns to server.php (Symfony's runtime ends the
     * script with exit) calls it itself.
     */
    function serve(App $app): void
    {
        if (Runtime::$mode === '') {
            throw new \LogicException('Cairo\Internal\serve(): the thread serves already, or is not a thread of cairo serve');
        }
        $mode = Runtime::$mode;
        Runtime::$mode = '';
        \cairo_spec(json_encode($app->spec(), JSON_THROW_ON_ERROR | JSON_UNESCAPED_SLASHES));
        if ($mode === 'serve') {
            (new Scheduler($app))->run();
        } elseif ($mode === 'offload') {
            (new OffloadWorker())->run();
        }
    }
}

namespace {
    require __DIR__ . '/app.php';
    require __DIR__ . '/context.php';
    require __DIR__ . '/channel.php';
    require __DIR__ . '/async.php';
    require __DIR__ . '/offload.php';
    require __DIR__ . '/waits.php';
    require __DIR__ . '/scheduler.php';
    require __DIR__ . '/go.php';
    \Cairo\Internal\main();
}
