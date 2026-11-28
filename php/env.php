<?php

/**
 * putenv() where one PHP request serves all of a thread's requests (fiber
 * and isolated mode): what it set lives in Env::$changes, which is
 * request-local, so each request starts from what the thread set before its
 * first one (server.php, an app's boot) and what a request sets ends with it,
 * as under PHP-FPM. internal/sapi/env_hook.c reads and writes it.
 */

declare(strict_types=1);

namespace Cairo\Internal {

    /** @internal */
    final class Env
    {
        /** @var array<string, ?string> what putenv() set, by name (null: unset) */
        public static array $changes = [];
    }

    Scheduler::onStart(static function (Scheduler $scheduler): void {
        if ($scheduler->app()->classic()) {
            return;   // each request is a PHP request, whose own table ends with it
        }
        \cairo_env_local(Env::class, 'changes');
        \cairo_local_register(Env::class, 'changes');
    });
}
