<?php
/**
 * What loads the runtime, and tells its files from the app's: runtime.php
 * starts with it, and an entry that serves a thread's requests with less
 * (Options.Entry) loads the rest of the runtime with it when it needs it.
 */

declare(strict_types=1);

namespace Cairo\Internal {

    /** Cairo's own PHP files: runtime.php, this one and those they load at start. */
    final class Runtime
    {
        /** @var array<string, true> */
        public static array $files = [];
        /** declare, serve or offload until serve() has run; '' after */
        public static string $mode = '';
        /** @var ?array<string, mixed> an event the entry took and left to the scheduler */
        public static ?array $first = null;

        /**
         * What this directory has loaded by now is the runtime; the server
         * file and what loads after it, Cairo's files included, counts as
         * the app.
         */
        public static function mark(): void
        {
            self::$files = array_fill_keys(array_filter(get_included_files(), static fn (string $file): bool => dirname($file) === __DIR__), true);
        }
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
        if (!class_exists(\Cairo\Context::class, false)) {
            return false;   // a classic thread's next PHP request, before it loaded the context: no request runs yet
        }
        $context = \Cairo\Context::current();
        return !$context->answered() && $context->err() instanceof \Cairo\Context\DeadlineExceeded;
    }

    /**
     * Loads the whole runtime: the core's files, then those the extensions
     * registered (RegisterPHP in internal/core). An entry that serves its
     * requests with less (Options.Entry) calls this when it needs the rest.
     *
     * @param array<string, mixed> $info what cairo_info() says of the thread
     */
    function load(array $info): void
    {
        foreach (['app', 'context', 'channel', 'async', 'offload', 'waits', 'scheduler', 'go'] as $file) {
            require_once __DIR__ . '/' . $file . '.php';
        }
        foreach ($info['php'] as $file) {
            require_once __DIR__ . '/' . $file;
        }
        Runtime::mark();
    }

    /**
     * Loads the files of the classes that load on first use (RegisterPHP
     * in internal/core) on that first use.
     *
     * @param array<string, mixed> $info what cairo_info() says of the thread
     * @return array<string, string> class => file
     */
    function autoload(array $info): array
    {
        $onUse = $info['classes'] + ['Cairo\\RequestLocal' => 'request_local.php'];
        spl_autoload_register(static function (string $class) use ($onUse): void {
            if (isset($onUse[$class])) {
                require_once __DIR__ . '/' . $onUse[$class];
            }
        });
        return $onUse;
    }
}
