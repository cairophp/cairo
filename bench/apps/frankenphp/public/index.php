<?php

// FrankenPHP worker mode: the script stays loaded and handles requests in a loop.

declare(strict_types=1);

require __DIR__ . '/../../router.php';

$handler = static function (): void {
    [$status, $type, $body] = bench_route(parse_url($_SERVER['REQUEST_URI'], PHP_URL_PATH));
    http_response_code($status);
    header("Content-Type: $type");
    echo $body;
};

while (frankenphp_handle_request($handler)) {
}
