<?php

// Waits of PHP's own past the deadline that CAIRO_TIMEOUT gives: the script
// waits with its thread, as under PHP-FPM, but no longer than the deadline,
// and the thread is free for the next request.

declare(strict_types=1);

if (isset($_GET['read'])) {
    // CAIRO_SILENT_PEER accepts the connection and never writes.
    $peer = stream_socket_client('tcp://' . getenv('CAIRO_SILENT_PEER'));
    fread($peer, 1);
} else {
    sleep(5);
}
