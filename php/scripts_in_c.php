<?php
/**
 * A classic app whose every route is scripts(): each request thread checks
 * the declaration in its first PHP request, and from its next one on serves
 * each script in C, as PHP's own SAPI request (internal/sapi/plain_request.c),
 * as `cairo serve` does with no declaration. No PHP of the app (server.php)
 * runs around a script, as under PHP-FPM.
 */

declare(strict_types=1);

namespace Cairo\Internal {

    Scheduler::onStart(static function (Scheduler $scheduler): void {
        $app = $scheduler->app();
        if (!$app->classic() || !$app->scriptsOnly()) {
            return;
        }
        $info = json_decode(\cairo_info(), true, 512, JSON_THROW_ON_ERROR);
        \cairo_plain_runtime(__DIR__, (bool) $info['reloaded']);
        if (\cairo_cserve()) {
            \cairo_thread_again();
            $scheduler->leave();
        }
    });
}
