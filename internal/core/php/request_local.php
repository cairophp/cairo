<?php

// Cairo\RequestLocal: static properties that keep one value per request.
// Loaded by runtime.php's autoloader on first use.

declare(strict_types=1);

namespace Cairo {

    /**
     * Static properties that hold one request's state, such as the current
     * tenant, user or app instance. A PHP thread runs many requests at once,
     * so a plain static is shared by all of them: one request would read the
     * value another one set. A registered static keeps a separate value for
     * each request instead:
     *
     *     RequestLocal::register(Tenant::class, 'current');   // in server.php
     *
     *     Tenant::$current = $tenant;   // in a handler: seen by this request only
     *
     * Each request starts with the value the property had when the server
     * started (objects are shared, arrays are copied on write). What a
     * request sets is seen by it and its async() tasks, and dropped when it
     * ends. Register in server.php, before the server starts; any
     * visibility works. Statics nobody registered stay shared, so a package
     * that keeps request state in one must have it registered.
     */
    final class RequestLocal
    {
        /** @var array<string, true> "Class::$property" of each registered static, by its declaring class */
        private static array $registered = [];

        /** Give each of $class's static $properties one value per request. */
        public static function register(string $class, string ...$properties): void
        {
            foreach ($properties as $property) {
                \cairo_local_register($class, $property);
                $declaring = (new \ReflectionProperty($class, $property))->getDeclaringClass()->getName();
                self::$registered[$declaring . '::$' . $property] = true;
            }
        }

        /**
         * @internal for the leak detector
         * @return array<string, true>
         */
        public static function registered(): array
        {
            return self::$registered;
        }
    }
}
