<?php
/**
 * Cairo runtime: where the PHP half of Cairo starts.
 *
 * Every PHP thread runs this file. It loads the files next to it, which hold
 * the Cairo\ classes and the fiber scheduler, and then the user's server.php,
 * which returns a Cairo\App describing the servers. On the boot thread that
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

    /**
     * Called inside the running fiber when Go's blocking detector found the
     * thread busy for $ms milliseconds (see cairo_report_blocking in cairo.c).
     */
    function reportBlocking(int $ms): void
    {
        if (!Scheduler::running() || Scheduler::instance()->app()->isolated()) {
            return;
        }
        // Frame i holds the line that called frame i's function, so the
        // function that line is in is frame i+1's. Frame 0's line is where
        // the request's code was when the interrupt ran.
        $trace = debug_backtrace(DEBUG_BACKTRACE_IGNORE_ARGS);
        $lines = [];
        foreach ($trace as $i => $frame) {
            if (!isset($frame['file']) || isset(Runtime::$files[$frame['file']])) {
                continue;   // internal calls, and Cairo's runtime
            }
            $in = $trace[$i + 1] ?? null;
            $fn = $in === null ? '{main}' : (isset($in['class']) ? $in['class'] . $in['type'] . $in['function'] : $in['function']) . '()';
            $lines[] = sprintf('  %s:%d in %s', $frame['file'], $frame['line'] ?? 0, $fn);
        }
        $request = \Cairo\Context::current()->requestId;
        error_log("cairo: blocking: request $request kept its PHP thread busy for {$ms}ms; other requests on the thread waited. At:\n" . implode("\n", $lines));
    }

    function main(): void
    {
        // What this directory has loaded by now is the runtime; server.php and
        // what loads after it, Cairo's files included, counts as the app.
        Runtime::$files = array_fill_keys(array_filter(get_included_files(), static fn (string $file): bool => dirname($file) === __DIR__), true);
        $info = json_decode(\cairo_info(), true, 512, JSON_THROW_ON_ERROR);
        // Cairo\Revolt\Driver implements Revolt's interface, so it can only be
        // declared once revolt/event-loop is loadable: on first use.
        spl_autoload_register(static function (string $class): void {
            $file = [
                'Cairo\\Cache' => 'cache.php', 'Cairo\\Pool' => 'pool.php', 'Cairo\\PoolTimeout' => 'pool.php',
                'Cairo\\Internal\\Queries' => 'queries.php', 'Cairo\\Internal\\Leaks' => 'leaks.php', 'Cairo\\RequestLocal' => 'request_local.php',
            ][$class] ?? null;
            if ($file !== null) {
                require_once __DIR__ . '/' . $file;
                return;
            }
            if (str_starts_with($class, 'Cairo\\Revolt\\')) {
                if (!interface_exists(\Revolt\EventLoop\Driver::class)) {
                    throw new \LogicException("$class needs revolt/event-loop: composer require revolt/event-loop, and load vendor/autoload.php first");
                }
                require_once __DIR__ . '/revolt.php';
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
    require __DIR__ . '/hooks.php';
    require __DIR__ . '/middleware.php';
    require __DIR__ . '/http.php';
    require __DIR__ . '/form.php';
    require __DIR__ . '/superglobals.php';
    require __DIR__ . '/http_adapter.php';
    require __DIR__ . '/http_client.php';
    require __DIR__ . '/grpc.php';
    require __DIR__ . '/grpc_client.php';
    require __DIR__ . '/net.php';
    require __DIR__ . '/sql.php';
    \Cairo\Internal\main();
}
