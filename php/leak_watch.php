<?php
/**
 * App::detectLeaks(): turns the leak detector on for a request thread whose
 * app asked for it. The detector itself is leaks.php, loaded on first use.
 */

declare(strict_types=1);

namespace Cairo\Internal {

    Scheduler::onStart(static function (Scheduler $scheduler): void {
        $ignored = $scheduler->app()->leaksIgnored();
        if ($ignored !== null) {
            Leaks::start($ignored);
            $scheduler->afterRequest(static fn (int $request, string $route) => Leaks::check($route));
        }
    });
}
