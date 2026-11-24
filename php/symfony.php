<?php
/**
 * Symfony's front controller (public/index.php, through autoload_runtime.php)
 * takes its runtime class from APP_RUNTIME: under cairo serve it is Cairo's
 * (symfony_runtime.php), unless the app names another.
 */

declare(strict_types=1);

namespace Cairo\Internal {

    if (PHP_SAPI === 'cairo') {
        $_SERVER['APP_RUNTIME'] ??= $_ENV['APP_RUNTIME'] ?? 'Cairo\Symfony\Runtime';
    }
}
