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
 */

declare(strict_types=1);

namespace Cairo\Internal {

    use Cairo\App;

    /** Cairo's own PHP files: this one and those it loads at start. */
    final class Runtime
    {
        /** @var array<string, true> */
        public static array $files = [];
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
        // Go names the files of the extensions (registerPHP in internal/sapi):
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
        $app = (static fn (string $file): mixed => require $file)($info['server']);
        if (!$app instanceof App) {
            throw new \RuntimeException($info['server'] . ' must return a Cairo\App');
        }
        \cairo_spec(json_encode($app->spec(), JSON_THROW_ON_ERROR | JSON_UNESCAPED_SLASHES));
        if ($info['mode'] === 'serve') {
            (new Scheduler($app))->run();
        } elseif ($info['mode'] === 'offload') {
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
    require __DIR__ . '/scheduler.php';
    require __DIR__ . '/go.php';
    \Cairo\Internal\main();
}
