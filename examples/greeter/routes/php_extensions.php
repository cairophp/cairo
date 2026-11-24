<?php

// The default PHP build: opcache and the wider set of extensions.

declare(strict_types=1);

use Cairo\Http\Request;
use Cairo\Http\Response;

return function ($http): void {
    $http->get('/php/extensions', function (Request $req): Response {
        $names = [
            'mbstring', 'intl', 'openssl', 'sodium', 'zip', 'gd', 'bcmath', 'gmp', 'ctype', 'dom',
            'fileinfo', 'tokenizer', 'pdo_sqlite', 'sqlite3', 'session', 'sockets', 'exif', 'iconv',
            'simplexml', 'xml', 'zlib', 'bz2', 'curl', 'pdo_mysql', 'mysqli',
        ];
        $loaded = [];
        foreach ($names as $name) {
            $loaded[$name] = extension_loaded($name);
        }
        $status = function_exists('opcache_get_status') ? opcache_get_status(false) : false;
        return Response::json([
            'sapi' => PHP_SAPI,
            // pcntl is for the scripts of `cairo php`: a server has none of its functions.
            'pcntl' => array_values(array_filter(get_defined_functions()['internal'], static fn (string $f): bool => str_starts_with($f, 'pcntl_'))),
            'extensions' => $loaded,
            'opcache' => [
                'loaded' => extension_loaded('Zend OPcache'),
                'enabled' => is_array($status) && $status['opcache_enabled'],
                'cached' => function_exists('opcache_is_script_cached') && opcache_is_script_cached(__FILE__),
                // Cairo's own PHP files: one of the core, one of an extension, one loaded on first use.
                'runtime' => array_map(
                    static fn (string $class): bool => function_exists('opcache_is_script_cached')
                        && opcache_is_script_cached((string) (new ReflectionClass($class))->getFileName()),
                    ['core' => Cairo\Context::class, 'extension' => Response::class, 'onUse' => Cairo\Pool::class],
                ),
            ],
            // A few calls that only work with the extensions in place.
            'checks' => [
                'mb_strtoupper' => extension_loaded('mbstring') ? mb_strtoupper('ümlaut') : null,
                'bcadd' => extension_loaded('bcmath') ? bcadd('0.1', '0.2', 1) : null,
                'sodium' => extension_loaded('sodium') ? strlen(sodium_crypto_secretbox_keygen()) : null,
                'intl' => extension_loaded('intl') ? (new NumberFormatter('en_US', NumberFormatter::DECIMAL))->format(1234567.5) : null,
                'sqlite' => extension_loaded('pdo_sqlite') ? (new PDO('sqlite::memory:'))->query('select 6*7')->fetchColumn() : null,
            ],
        ]);
    });
};
