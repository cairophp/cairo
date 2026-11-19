<?php

// The leak detector (App::detectLeaks()): finds static properties that hold
// request state but are not request-local. Loaded by runtime.php's
// autoloader on first use.

declare(strict_types=1);

namespace Cairo\Internal {

    use Cairo\RequestLocal;

    /** @internal */
    final class Leaks
    {
        /** Values read per static: past it, the rest is not compared. */
        private const BUDGET = 2000;
        /** Levels of arrays and objects read below the static. */
        private const DEPTH = 3;
        /** Strings longer than this are compared by hash. */
        private const MAX_STRING = 64;
        /** What a value that was never set reads as: setting it is not a change. */
        private const UNSET = ['N' => true, 'L0' => true, 'u' => true];

        /** @var list<array{\ReflectionProperty, string}> declared, unregistered statics and their names */
        private static array $statics = [];
        /** Classes looked at so far, in get_declared_classes() order. */
        private static int $classes = 0;
        /** @var array<string, array<string, string>> name => path in the value => what is there */
        private static array $last = [];
        /** @var array<string, true> statics that changed once: a setting or lazy value is set once */
        private static array $changedOnce = [];
        /** @var array<string, true> statics not to read */
        private static array $ignored = [];

        /**
         * Takes the first snapshot, once the app's statics are set up.
         *
         * @param list<string> $ignored "Class::$property"
         */
        public static function start(array $ignored): void
        {
            foreach ($ignored as $name) {
                self::$ignored[ltrim($name, '\\')] = true;
            }
            self::check(null);
        }

        /** Called when a request ends: reports the statics that changed since the last check. */
        public static function check(?string $route): void
        {
            self::discover();
            $leaked = [];
            foreach (self::$statics as [$property, $name]) {
                $budget = self::BUDGET;
                $now = [];
                if ($property->isInitialized()) {
                    self::read($property->getValue(), '', self::DEPTH, $budget, $now);
                } else {
                    $now[''] = 'u';
                }
                if (isset(self::$last[$name]) && self::changed(self::$last[$name], $now)) {
                    if (isset(self::$changedOnce[$name])) {
                        $leaked[] = $name;
                    }
                    self::$changedOnce[$name] = true;
                }
                self::$last[$name] = $now;
            }
            if ($leaked !== [] && $route !== null) {
                Go::callNow('leaks.report', ['route' => $route, 'statics' => $leaked]);
            }
        }

        /**
         * Whether a value that was set was replaced, or a list grew. Setting
         * an empty place, adding keys to a map (a cache), and removing or
         * clearing entries (a reset) are not changes.
         *
         * @param array<string, string> $was
         * @param array<string, string> $now
         */
        private static function changed(array $was, array $now): bool
        {
            foreach ($was as $path => $before) {
                $after = $now[$path] ?? null;
                if ($after === null || $after === $before || $after === 'L0' || isset(self::UNSET[$before])) {
                    continue;
                }
                if ($before[0] === 'L' && $after[0] === 'L') {
                    if ((int) substr($after, 1) > (int) substr($before, 1)) {
                        return true;
                    }
                    continue;
                }
                return true;
            }
            return false;
        }

        /**
         * Adds what $value holds to $out, by path, reading at most $budget values.
         *
         * @param array<string, string> $out
         */
        private static function read(mixed $value, string $path, int $depth, int &$budget, array &$out): void
        {
            if (--$budget < 0) {
                return;
            }
            if (is_array($value)) {
                $out[$path] = array_is_list($value) ? 'L' . count($value) : 'M';
                $items = $value;
            } elseif (is_object($value)) {
                $out[$path] = 'o' . spl_object_id($value) . get_class($value);
                $items = $value instanceof \Closure ? [] : (array) $value;
            } else {
                $out[$path] = self::scalar($value);
                return;
            }
            if ($depth === 0) {
                return;
            }
            foreach ($items as $key => $item) {
                self::read($item, $path . "\0" . $key, $depth - 1, $budget, $out);
                if ($budget < 0) {
                    return;
                }
            }
        }

        private static function scalar(mixed $value): string
        {
            if ($value === null) {
                return 'N';
            }
            if (is_string($value)) {
                return strlen($value) > self::MAX_STRING ? 's' . hash('xxh128', $value) : 's' . $value;
            }
            if (is_resource($value)) {
                return 'r' . get_resource_id($value);
            }
            return 'v' . var_export($value, true);
        }

        /** Adds the statics of the classes declared since the last call. */
        private static function discover(): void
        {
            $classes = get_declared_classes();
            $registered = RequestLocal::registered();
            foreach (array_slice($classes, self::$classes) as $class) {
                $info = new \ReflectionClass($class);
                $file = $info->getFileName();
                if ($file === false || dirname($file) === __DIR__) {
                    continue;   // PHP's own classes, and Cairo's runtime
                }
                foreach ($info->getProperties(\ReflectionProperty::IS_STATIC) as $property) {
                    $name = $class . '::$' . $property->getName();
                    $own = $property->getDeclaringClass()->getName() === $class;   // inherited ones share it
                    if ($own && !isset($registered[$name]) && !isset(self::$ignored[$name])) {
                        self::$statics[] = [$property, $name];
                    }
                }
            }
            self::$classes = count($classes);
        }
    }
}
