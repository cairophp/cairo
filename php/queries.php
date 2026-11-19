<?php

// The repeated query detector (App::repeatedQueries()): counts each
// request's statements by shape, and reports the ones that ran too often
// when the request ends. Loaded by runtime.php's autoloader on first use.

declare(strict_types=1);

namespace Cairo\Internal {

    use Cairo\Context;

    /** @internal */
    final class Queries
    {
        /** Distinct statements counted per request; more are not counted. */
        private const MAX_SHAPES = 100;
        /** Bytes of a statement that are read, and of a shape that are kept. */
        private const MAX_SQL = 4096;
        private const MAX_SHAPE = 300;
        private const SHAPE = [
            '/\'(?:[^\'\\\\]|\\\\.|\'\')*\'/s' => '?',             // 'string'
            '/(?<![\w$.])\d+(?:\.\d+)?(?:e[+-]?\d+)?\b/i' => '?',  // 42, 4.2, 1e3 (not t1 or $1)
            '/\(\s*\?(?:\s*,\s*\?)*\s*\)/' => '(?)',               // IN (?, ?, ?)
            '/\s+/' => ' ',
        ];

        /** Statements whose shape is remembered on this thread, and how long they may be. */
        private const MAX_CACHED = 512;
        private const MAX_CACHED_SQL = 1024;

        /** @var array<int, array<string, int>> request id => shape => times run */
        private static array $seen = [];
        /** @var array<string, string> statement => shape: a statement usually runs many times */
        private static array $shapes = [];

        /** Counts $sql for the request of the running fiber. */
        public static function seen(string $sql): void
        {
            $id = Context::current()->requestId;
            if ($id === 0) {
                return;   // not in a request
            }
            $shape = self::$shapes[$sql] ?? self::remember($sql);
            if ($shape === '') {
                return;
            }
            if (!isset(self::$seen[$id][$shape]) && count(self::$seen[$id] ?? []) >= self::MAX_SHAPES) {
                return;
            }
            self::$seen[$id][$shape] = (self::$seen[$id][$shape] ?? 0) + 1;
        }

        /** Reports request $id's statements that ran $times or more, and forgets the request. */
        public static function end(int $id, string $route, int $times): void
        {
            $seen = self::$seen[$id] ?? [];
            unset(self::$seen[$id]);
            $repeated = [];
            foreach ($seen as $shape => $n) {
                if ($n >= $times) {
                    $repeated[] = ['query' => (string) $shape, 'count' => $n];
                }
            }
            if ($repeated !== []) {
                Go::callNow('sql.repeated', ['route' => $route, 'queries' => $repeated]);
            }
        }

        private static function remember(string $sql): string
        {
            $shape = self::shape($sql);
            if (strlen($sql) <= self::MAX_CACHED_SQL) {
                if (count(self::$shapes) >= self::MAX_CACHED) {
                    self::$shapes = [];
                }
                self::$shapes[$sql] = $shape;
            }
            return $shape;
        }

        /** $sql without its literals, or '' when they cannot be told apart. */
        private static function shape(string $sql): string
        {
            $shape = preg_replace(array_keys(self::SHAPE), array_values(self::SHAPE), substr($sql, 0, self::MAX_SQL));
            if ($shape === null) {
                return '';
            }
            // A string cut off at MAX_SQL is still open: drop the rest.
            if (($quote = strpos($shape, "'")) !== false) {
                $shape = substr($shape, 0, $quote) . '?';
            }
            return substr(trim($shape), 0, self::MAX_SHAPE);
        }
    }
}
