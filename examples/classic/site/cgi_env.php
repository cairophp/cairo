<?php

// A request's CGI variables are its environment, as under PHP-FPM: getenv()
// finds them before what putenv() set, getenv() with no name and $_ENV list
// them, and a change to $_SERVER does not change them.

putenv('REQUEST_METHOD=PUT');
$_SERVER['CLASSIC_SERVER_ONLY'] = 'x';
$env = str_contains((string) ini_get('variables_order'), 'E')
    ? ($_ENV['HTTP_X_CLASSIC'] ?? false)
    : getenv('HTTP_X_CLASSIC');

echo json_encode([
    'method' => getenv('REQUEST_METHOD'),
    'local' => getenv('REQUEST_METHOD', true),
    'header' => getenv('HTTP_X_CLASSIC'),
    'listed' => getenv()['HTTP_X_CLASSIC'] ?? false,
    'env' => $env,
    'server_only' => getenv('CLASSIC_SERVER_ONLY'),
    'php_self' => getenv('PHP_SELF'),
    'https' => getenv('HTTPS'),
]), "\n";
