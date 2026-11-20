<?php
/**
 * App::repeatedQueries(): turns the repeated query detector on for a request
 * thread whose app asked for it. The detector itself is queries.php, loaded
 * on first use.
 */

declare(strict_types=1);

namespace Cairo\Internal {

    /** @internal Whether this thread counts statements: Cairo\Sql asks before it reports one. */
    final class QueryWatch
    {
        public static bool $on = false;
    }

    Scheduler::onStart(static function (Scheduler $scheduler): void {
        $times = $scheduler->app()->repeatedQueriesAt();
        QueryWatch::$on = $times > 0;
        \cairo_watch_queries(QueryWatch::$on);
        if (QueryWatch::$on) {
            $scheduler->afterRequest(static fn (int $request, string $route) => Queries::end($request, $route, $times));
        }
    });
}
