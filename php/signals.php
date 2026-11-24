<?php
/**
 * Signals in a script of `cairo php`: the fiber that runs the script's
 * handlers (pcntl_signal()) while no other fiber of the script runs.
 */

declare(strict_types=1);

namespace Cairo\Internal {

    use Cairo\Context;

    /** @internal */
    final class Signals
    {
        /**
         * Called by the thread's interrupt while a signal is pending:
         * whether the code that runs, in $file, may be interrupted by a
         * handler. Cairo's own runtime may not: a handler that throws, or
         * calls into the runtime, between two of its steps would leave one
         * half done.
         */
        public static function interruptible(?string $file): bool
        {
            return $file === null || !isset(Runtime::$files[$file]);
        }

        /**
         * Called by pcntl_signal() (internal/sapi/signals.c) for the first
         * handler of a script that runs under the scheduler. A signal that
         * arrives while a fiber runs PHP is handled there, at its next safe
         * point. While every fiber waits, Go wakes the fiber started here,
         * which raises the signal: pcntl runs the script's handlers in it.
         * pcntl lets no fiber switch inside a handler, so a handler waits
         * only where the script waits with its thread.
         */
        public static function watch(): void
        {
            if (!Scheduler::running() || \Fiber::getCurrent() === null) {
                return;
            }
            $scheduler = Scheduler::instance();
            // Under the script itself and not under the context of whoever
            // asks: a task, or a deadline, ends before the script does.
            $context = new Context(Context::current()->requestId, null);
            $scheduler->startFiber(static function () use ($scheduler): void {
                \cairo_signals_watch(true);
                try {
                    while (true) {
                        try {
                            $scheduler->callAside('signals.wait', []);
                        } catch (Context\Canceled) {
                            return;   // the script has ended
                        }
                        \cairo_signals_raise();
                    }
                } finally {
                    // From here a signal goes to PHP's handler on arrival, for
                    // what still runs: the shutdown functions.
                    \cairo_signals_watch(false);
                }
            }, $context);
        }
    }
}
