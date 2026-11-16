<?php

// The cairo SAPI: its name, php.ini handling and logging.

declare(strict_types=1);

use Cairo\App;
use Cairo\Http\Request;
use Cairo\Http\Response;

$app = new App();
$http = $app->http((string) getenv('CAIRO_HTTP'));

$http->get('/sapi', fn (): Response => Response::json([
    'sapi' => PHP_SAPI,
    'php_sapi_name' => php_sapi_name(),
    'ini_file' => php_ini_loaded_file(),
    'memory_limit' => ini_get('memory_limit'),
    'max_execution_time' => ini_get('max_execution_time'),
    'display_errors' => ini_get('display_errors'),
    'dl' => function_exists('dl'),
]));

$http->get('/log', function (Request $req): Response {
    error_log('hello from php: ' . $req->query('msg'));
    trigger_error('a warning from php', E_USER_WARNING);
    return Response::text("logged\n");
});

return $app;
