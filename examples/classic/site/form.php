<?php

// The request as PHP gives it to a script: superglobals, php://input,
// PATH_INFO, cookies; and the answer: header(), setcookie(), echo.

header('X-Classic: yes');
setcookie('seen', '1');
echo json_encode([
    'method' => $_SERVER['REQUEST_METHOD'],
    'get' => $_GET,
    'post' => $_POST,
    'input' => file_get_contents('php://input'),
    'path_info' => $_SERVER['PATH_INFO'] ?? null,
    'self' => $_SERVER['PHP_SELF'],
    'cookie' => $_COOKIE,
]), "\n";
