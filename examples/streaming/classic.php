<?php

// Classic PHP in isolated mode: echo and header() are the answer, and
// flush() sends what is written so far, as under any PHP server.

declare(strict_types=1);

use Cairo\App;

$app = (new App())->concurrency('isolated');
$http = $app->http(getenv('CAIRO_HTTP') ?: ':8080');

// The client has "first" before the handler has made "second".
$http->get('/flush', function (): void {
    header('Content-Type: text/plain');
    header('X-Classic: yes');
    echo "first\n";
    flush();
    usleep((int) (1e6 * (float) ($_GET['wait'] ?? '1')));
    echo "second\n";
});

// What frameworks do before they stream: every buffer is closed first.
$http->get('/unbuffered', function (): void {
    header('Content-Type: text/plain');
    while (ob_get_level() > 0) {
        ob_end_flush();
    }
    echo "one\n";
    flush();
    echo "two\n";
    $late = @header('X-Late: no');   // PHP refuses: the head is with the client
    echo headers_sent() ? "headers were sent\n" : "headers were not sent\n";
});

// No flush(): one answer, with its length.
$http->get('/whole', function (): void {
    ob_start();
    echo 'in a buffer of its own, ';
    ob_flush();
    echo 'left open';
});

// Early Hints, the way Symfony and plain PHP send them.
$http->get('/hints', function (): void {
    header('Link: </style.css>; rel=preload; as=style');
    headers_send(103);
    usleep(100_000);
    echo '<link rel="stylesheet" href="/style.css">';
});

// The answer is whole before the slow part runs.
$http->get('/finish', function (): void {
    echo "accepted\n";
    $finished = fastcgi_finish_request();
    usleep(300_000);
    echo 'nobody reads this';
    file_put_contents((string) getenv('CAIRO_FINISHED'), $finished ? "finished, then worked\n" : "not finished\n");
});

return $app;
