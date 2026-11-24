<?php

// Symfony's own front controller, unchanged: `cairo serve public/index.php`
// serves it with no server.php. Cairo's runtime (Cairo\Symfony\Runtime) is
// the one autoload_runtime.php picks under cairo serve; its options come
// from APP_RUNTIME_OPTIONS or extra.runtime in composer.json:
//
//   APP_RUNTIME_OPTIONS='{"listen":":8080","concurrency":"fibers"}' cairo serve public/index.php
//
// Run `composer install` here first.

use App\Kernel;

require_once dirname(__DIR__) . '/vendor/autoload_runtime.php';

return static fn (array $context): Kernel => new Kernel($context['APP_ENV'], (bool) $context['APP_DEBUG']);
