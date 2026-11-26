<?php

// headers_send(103) sends the headers set so far as Early Hints, which a
// browser preloads from while the page is made; they stay for the answer.

declare(strict_types=1);

header('Link: </style.css>; rel=preload; as=style');
headers_send(103);
echo 'page user=', $_SERVER['PHP_AUTH_USER'] ?? '-', "\n";
