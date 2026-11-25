<?php

// Classic PHP: a PHP request of its own for every HTTP request, as under
// PHP-FPM, for apps that are a set of scripts (WordPress, Drupal, a folder
// of .php files). scripts() maps each request to a script under site/, as
// nginx's try_files does in front of PHP-FPM, and Go serves the other files.

declare(strict_types=1);

use Cairo\App;

$app = (new App())
    ->concurrency('classic')
    ->threads((int) (getenv('CAIRO_THREADS') ?: 4));

$app->http(getenv('CAIRO_HTTP') ?: ':8080')->scripts(__DIR__ . '/site');

return $app;
