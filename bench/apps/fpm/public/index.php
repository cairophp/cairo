<?php

declare(strict_types=1);

require __DIR__ . '/../../router.php';

[$status, $type, $body] = bench_route(parse_url($_SERVER['REQUEST_URI'], PHP_URL_PATH));
http_response_code($status);
header("Content-Type: $type");
echo $body;
