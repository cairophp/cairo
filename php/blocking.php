<?php
/**
 * App::detectBlocking(): the report a request thread makes of the code that
 * kept it busy.
 */

declare(strict_types=1);

namespace Cairo\Internal {

    /**
     * Called inside the running fiber when Go's blocking detector found the
     * thread busy for $ms milliseconds (see internal/sapi/blocking.c).
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
}
